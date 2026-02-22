package controller

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/task"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/inbound"
	"github.com/xtls/xray-core/features/outbound"
	"github.com/xtls/xray-core/features/policy"
	"github.com/xtls/xray-core/features/stats"

	"github.com/XrayR-project/XrayR/api"
	"github.com/XrayR-project/XrayR/app/mydispatcher"
	"github.com/XrayR-project/XrayR/common/mylego"
	"github.com/XrayR-project/XrayR/common/serverstatus"
)

type LimitInfo struct {
	end               int64
	currentSpeedLimit int
	originSpeedLimit  uint64
	user              api.UserInfo
}

type Controller struct {
	server       *core.Instance
	config       *Config
	clientInfo   api.ClientInfo
	apiClient    api.API
	nodeInfo     *api.NodeInfo
	Tag          string
	userList     *[]api.UserInfo
	tasks        []periodicTask
	limitedUsers map[int]LimitInfo
	warnedUsers  map[int]int
	// Cache per-user traffic counters to avoid repeated stats manager lookups.
	trafficCounterCache *sync.Map
	trafficSweepTick    int
	panelType           string
	ibm                 inbound.Manager
	obm                 outbound.Manager
	stm                 stats.Manager
	pm                  policy.Manager
	dispatcher          *mydispatcher.DefaultDispatcher
	startAt             time.Time
	logger              *log.Entry
}

type periodicTask struct {
	tag string
	*task.Periodic
}

const fastTrafficFullSweepInterval = 5

// New return a Controller service with default parameters.
func New(server *core.Instance, api api.API, config *Config, panelType string) *Controller {
	logger := log.NewEntry(log.StandardLogger()).WithFields(log.Fields{
		"Host": api.Describe().APIHost,
		"Type": api.Describe().NodeType,
		"ID":   api.Describe().NodeID,
	})
	controller := &Controller{
		server:              server,
		config:              config,
		apiClient:           api,
		panelType:           panelType,
		trafficCounterCache: new(sync.Map),
		ibm:                 server.GetFeature(inbound.ManagerType()).(inbound.Manager),
		obm:                 server.GetFeature(outbound.ManagerType()).(outbound.Manager),
		stm:                 server.GetFeature(stats.ManagerType()).(stats.Manager),
		pm:                  server.GetFeature(policy.ManagerType()).(policy.Manager),
		dispatcher:          server.GetFeature(mydispatcher.Type()).(*mydispatcher.DefaultDispatcher),
		startAt:             time.Now(),
		logger:              logger,
	}

	return controller
}

// Start implement the Start() function of the service interface
func (c *Controller) Start() error {
	c.clientInfo = c.apiClient.Describe()
	// First fetch Node Info
	newNodeInfo, err := c.apiClient.GetNodeInfo()
	if err != nil {
		return err
	}
	if newNodeInfo.Port == 0 {
		return errors.New("server port must > 0")
	}
	c.nodeInfo = newNodeInfo
	c.Tag = c.buildNodeTag()

	// Add new tag
	err = c.addNewTag(newNodeInfo)
	if err != nil {
		c.logger.Panic(err)
		return err
	}
	// Update user
	userInfo, err := c.apiClient.GetUserList()
	if err != nil {
		return err
	}

	// sync controller userList
	c.userList = userInfo

	err = c.addNewUser(userInfo, newNodeInfo)
	if err != nil {
		return err
	}

	// Add Limiter
	if err := c.AddInboundLimiter(c.Tag, newNodeInfo.SpeedLimit, userInfo, c.config.GlobalDeviceLimitConfig); err != nil {
		c.logger.Print(err)
	}

	// Add Rule Manager
	if !c.config.DisableGetRule {
		if ruleList, err := c.apiClient.GetNodeRule(); err != nil {
			c.logger.Printf("Get rule list filed: %s", err)
		} else if len(*ruleList) > 0 {
			if err := c.UpdateRule(c.Tag, *ruleList); err != nil {
				c.logger.Print(err)
			}
		}
	}

	// Init AutoSpeedLimitConfig
	if c.config.AutoSpeedLimitConfig == nil {
		c.config.AutoSpeedLimitConfig = &AutoSpeedLimitConfig{0, 0, 0, 0}
	}
	if c.config.AutoSpeedLimitConfig.Limit > 0 {
		capHint := len(*userInfo)
		c.limitedUsers = make(map[int]LimitInfo, capHint)
		c.warnedUsers = make(map[int]int, capHint)
	}

	// Add periodic tasks
	c.tasks = append(c.tasks,
		periodicTask{
			tag: "node monitor",
			Periodic: &task.Periodic{
				Interval: time.Duration(c.config.UpdatePeriodic) * time.Second,
				Execute:  c.nodeInfoMonitor,
			}},
		periodicTask{
			tag: "user monitor",
			Periodic: &task.Periodic{
				Interval: time.Duration(c.config.UpdatePeriodic) * time.Second,
				Execute:  c.userInfoMonitor,
			}},
	)

	// Check cert service in need
	if c.nodeInfo.EnableTLS && c.config.EnableREALITY == false {
		c.tasks = append(c.tasks, periodicTask{
			tag: "cert monitor",
			Periodic: &task.Periodic{
				Interval: time.Duration(c.config.UpdatePeriodic) * time.Second * 60,
				Execute:  c.certMonitor,
			}})
	}

	// Start periodic tasks
	for i := range c.tasks {
		c.logger.Printf("Start %s periodic task", c.tasks[i].tag)
		go c.tasks[i].Start()
	}

	return nil
}

// Close implement the Close() function of the service interface
func (c *Controller) Close() error {
	for i := range c.tasks {
		if c.tasks[i].Periodic != nil {
			if err := c.tasks[i].Periodic.Close(); err != nil {
				c.logger.Panicf("%s periodic task close failed: %s", c.tasks[i].tag, err)
			}
		}
	}

	return nil
}

func (c *Controller) nodeInfoMonitor() (err error) {
	// delay to start
	if time.Since(c.startAt) < time.Duration(c.config.UpdatePeriodic)*time.Second {
		return nil
	}

	// First fetch Node Info
	var nodeInfoChanged = true
	newNodeInfo, err := c.apiClient.GetNodeInfo()
	if err != nil {
		if err.Error() == api.NodeNotModified {
			nodeInfoChanged = false
			newNodeInfo = c.nodeInfo
		} else {
			c.logger.Print(err)
			return nil
		}
	}
	if newNodeInfo.Port == 0 {
		return errors.New("server port must > 0")
	}

	// Update User
	var usersChanged = true
	newUserInfo, err := c.apiClient.GetUserList()
	if err != nil {
		if err.Error() == api.UserNotModified {
			usersChanged = false
			newUserInfo = c.userList
		} else {
			c.logger.Print(err)
			return nil
		}
	}

	// If nodeInfo changed
	if nodeInfoChanged {
		if !reflect.DeepEqual(c.nodeInfo, newNodeInfo) {
			// Remove old tag
			oldTag := c.Tag
			err := c.removeOldTag(oldTag)
			if err != nil {
				c.logger.Print(err)
				return nil
			}
			if c.nodeInfo.NodeType == "Shadowsocks-Plugin" {
				err = c.removeOldTag(fmt.Sprintf("dokodemo-door_%s+1", c.Tag))
			}
			if err != nil {
				c.logger.Print(err)
				return nil
			}
			// Add new tag
			c.nodeInfo = newNodeInfo
			c.Tag = c.buildNodeTag()
			err = c.addNewTag(newNodeInfo)
			if err != nil {
				c.logger.Print(err)
				return nil
			}
			nodeInfoChanged = true
			// Remove Old limiter
			if err = c.DeleteInboundLimiter(oldTag); err != nil {
				c.logger.Print(err)
				return nil
			}
		} else {
			nodeInfoChanged = false
		}
	}

	// Check Rule
	if !c.config.DisableGetRule {
		if ruleList, err := c.apiClient.GetNodeRule(); err != nil {
			if err.Error() != api.RuleNotModified {
				c.logger.Printf("Get rule list filed: %s", err)
			}
		} else if len(*ruleList) > 0 {
			if err := c.UpdateRule(c.Tag, *ruleList); err != nil {
				c.logger.Print(err)
			}
		}
	}

	if nodeInfoChanged {
		c.trafficCounterCache.Range(func(key, value interface{}) bool {
			c.trafficCounterCache.Delete(key)
			return true
		})
		c.ResetUserTrafficCounterCache()
		err = c.addNewUser(newUserInfo, newNodeInfo)
		if err != nil {
			c.logger.Print(err)
			return nil
		}

		// Add Limiter
		if err := c.AddInboundLimiter(c.Tag, newNodeInfo.SpeedLimit, newUserInfo, c.config.GlobalDeviceLimitConfig); err != nil {
			c.logger.Print(err)
			return nil
		}

	} else {
		var deleted, added []api.UserInfo
		if usersChanged {
			deleted, added = compareUserList(c.userList, newUserInfo)
			if len(deleted) > 0 {
				deletedEmail := make([]string, len(deleted))
				for i, u := range deleted {
					deletedEmail[i] = c.buildUserTag(&u)
					c.trafficCounterCache.Delete(deletedEmail[i])
					c.DropUserTrafficCounterCache(deletedEmail[i])
				}
				err := c.removeUsers(deletedEmail, c.Tag)
				if err != nil {
					c.logger.Print(err)
				}
			}
			if len(added) > 0 {
				err = c.addNewUser(&added, c.nodeInfo)
				if err != nil {
					c.logger.Print(err)
				}
				// Update Limiter
				if err := c.UpdateInboundLimiter(c.Tag, &added); err != nil {
					c.logger.Print(err)
				}
			}
		}
		c.logger.Printf("%d user deleted, %d user added", len(deleted), len(added))
	}
	c.userList = newUserInfo
	return nil
}

func (c *Controller) removeOldTag(oldTag string) (err error) {
	err = c.removeInbound(oldTag)
	if err != nil {
		return err
	}
	err = c.removeOutbound(oldTag)
	if err != nil {
		return err
	}
	return nil
}

func (c *Controller) addNewTag(newNodeInfo *api.NodeInfo) (err error) {
	if newNodeInfo.NodeType != "Shadowsocks-Plugin" {
		inboundConfig, err := InboundBuilder(c.config, newNodeInfo, c.Tag)
		if err != nil {
			return err
		}
		err = c.addInbound(inboundConfig)
		if err != nil {

			return err
		}
		outBoundConfig, err := OutboundBuilder(c.config, newNodeInfo, c.Tag)
		if err != nil {

			return err
		}
		err = c.addOutbound(outBoundConfig)
		if err != nil {

			return err
		}

	} else {
		return c.addInboundForSSPlugin(*newNodeInfo)
	}
	return nil
}

func (c *Controller) addInboundForSSPlugin(newNodeInfo api.NodeInfo) (err error) {
	// Shadowsocks-Plugin require a separate inbound for other TransportProtocol likes: ws, grpc
	fakeNodeInfo := newNodeInfo
	fakeNodeInfo.TransportProtocol = "tcp"
	fakeNodeInfo.EnableTLS = false
	// Add a regular Shadowsocks inbound and outbound
	inboundConfig, err := InboundBuilder(c.config, &fakeNodeInfo, c.Tag)
	if err != nil {
		return err
	}
	err = c.addInbound(inboundConfig)
	if err != nil {

		return err
	}
	outBoundConfig, err := OutboundBuilder(c.config, &fakeNodeInfo, c.Tag)
	if err != nil {

		return err
	}
	err = c.addOutbound(outBoundConfig)
	if err != nil {

		return err
	}
	// Add an inbound for upper streaming protocol
	fakeNodeInfo = newNodeInfo
	fakeNodeInfo.Port++
	fakeNodeInfo.NodeType = "dokodemo-door"
	dokodemoTag := fmt.Sprintf("dokodemo-door_%s+1", c.Tag)
	inboundConfig, err = InboundBuilder(c.config, &fakeNodeInfo, dokodemoTag)
	if err != nil {
		return err
	}
	err = c.addInbound(inboundConfig)
	if err != nil {

		return err
	}
	outBoundConfig, err = OutboundBuilder(c.config, &fakeNodeInfo, dokodemoTag)
	if err != nil {

		return err
	}
	err = c.addOutbound(outBoundConfig)
	if err != nil {

		return err
	}
	return nil
}

func (c *Controller) addNewUser(userInfo *[]api.UserInfo, nodeInfo *api.NodeInfo) (err error) {
	users := make([]*protocol.User, 0)
	switch nodeInfo.NodeType {
	case "V2ray", "Vmess", "Vless":
		if nodeInfo.EnableVless || (nodeInfo.NodeType == "Vless" && nodeInfo.NodeType != "Vmess") {
			users = c.buildVlessUser(userInfo)
		} else {
			users = c.buildVmessUser(userInfo)
		}
	case "Trojan":
		users = c.buildTrojanUser(userInfo)
	case "Shadowsocks":
		users = c.buildSSUser(userInfo, nodeInfo.CypherMethod)
	case "Shadowsocks-Plugin":
		users = c.buildSSPluginUser(userInfo)
	default:
		return fmt.Errorf("unsupported node type: %s", nodeInfo.NodeType)
	}

	err = c.addUsers(users, c.Tag)
	if err != nil {
		return err
	}
	c.logger.Printf("Added %d new users", len(*userInfo))
	return nil
}

func compareUserList(old, new *[]api.UserInfo) (deleted, added []api.UserInfo) {
	if old == nil || len(*old) == 0 {
		if new != nil {
			added = append(added, (*new)...)
		}
		return deleted, added
	}
	if new == nil || len(*new) == 0 {
		deleted = append(deleted, (*old)...)
		return deleted, added
	}

	type userIdentity struct {
		uid   int
		email string
	}

	oldByIdentity := make(map[userIdentity]api.UserInfo, len(*old))
	for _, user := range *old {
		oldByIdentity[userIdentity{uid: user.UID, email: user.Email}] = user
	}

	seen := make(map[userIdentity]struct{}, len(*new))
	for _, user := range *new {
		identity := userIdentity{uid: user.UID, email: user.Email}
		seen[identity] = struct{}{}

		if oldUser, ok := oldByIdentity[identity]; !ok {
			added = append(added, user)
		} else if oldUser != user {
			// Keep old behavior: modified users are treated as delete+add so downstream updates apply.
			deleted = append(deleted, oldUser)
			added = append(added, user)
		}
	}

	for identity, oldUser := range oldByIdentity {
		if _, ok := seen[identity]; !ok {
			deleted = append(deleted, oldUser)
		}
	}

	return deleted, added
}

func limitUser(c *Controller, user api.UserInfo, silentUsers *[]api.UserInfo) {
	endAt := time.Now().Unix() + int64(c.config.AutoSpeedLimitConfig.LimitDuration*60)
	c.limitedUsers[user.UID] = LimitInfo{
		end:               endAt,
		currentSpeedLimit: c.config.AutoSpeedLimitConfig.LimitSpeed,
		originSpeedLimit:  user.SpeedLimit,
		user:              user,
	}
	c.logger.Printf("Limit User: %s Speed: %d End: %s", c.buildUserTag(&user), c.config.AutoSpeedLimitConfig.LimitSpeed, time.Unix(endAt, 0).Format("01-02 15:04:05"))
	user.SpeedLimit = uint64((c.config.AutoSpeedLimitConfig.LimitSpeed * 1000000) / 8)
	*silentUsers = append(*silentUsers, user)
}

func parseUserTag(userTag string) (uid int, email string, ok bool) {
	last := strings.LastIndexByte(userTag, '|')
	if last <= 0 || last+1 >= len(userTag) {
		return 0, "", false
	}
	prev := strings.LastIndexByte(userTag[:last], '|')
	if prev < 0 || prev+1 >= last {
		return 0, "", false
	}
	parsedUID, err := strconv.Atoi(userTag[last+1:])
	if err != nil {
		return 0, "", false
	}
	return parsedUID, userTag[prev+1 : last], true
}

func (c *Controller) userInfoMonitor() (err error) {
	// delay to start
	if time.Since(c.startAt) < time.Duration(c.config.UpdatePeriodic)*time.Second {
		return nil
	}

	// Get server status
	CPU, Mem, Disk, Uptime, err := serverstatus.GetSystemInfo()
	if err != nil {
		c.logger.Print(err)
	}
	err = c.apiClient.ReportNodeStatus(
		&api.NodeStatus{
			CPU:    CPU,
			Mem:    Mem,
			Disk:   Disk,
			Uptime: Uptime,
		})
	if err != nil {
		c.logger.Print(err)
	}
	// Unlock users
	if c.config.AutoSpeedLimitConfig.Limit > 0 && len(c.limitedUsers) > 0 {
		nowUnix := time.Now().Unix()
		c.logger.Printf("Limited users:")
		toReleaseUsers := make([]api.UserInfo, 0, len(c.limitedUsers))
		for uid, limitInfo := range c.limitedUsers {
			user := limitInfo.user
			if nowUnix > limitInfo.end {
				user.SpeedLimit = limitInfo.originSpeedLimit
				toReleaseUsers = append(toReleaseUsers, user)
				c.logger.Printf("User: %s Speed: %d End: nil (Unlimit)", c.buildUserTag(&user), user.SpeedLimit)
				delete(c.limitedUsers, uid)
			} else {
				c.logger.Printf("User: %s Speed: %d End: %s", c.buildUserTag(&user), limitInfo.currentSpeedLimit, time.Unix(limitInfo.end, 0).Format("01-02 15:04:05"))
			}
		}
		if len(toReleaseUsers) > 0 {
			if err := c.UpdateInboundLimiter(c.Tag, &toReleaseUsers); err != nil {
				c.logger.Print(err)
			}
		}
	}

	// Get User traffic
	c.trafficSweepTick++
	userCount := len(*c.userList)
	userTraffic := make([]api.UserTraffic, 0, userCount)
	upCounterList := make([]stats.Counter, 0, userCount)
	downCounterList := make([]stats.Counter, 0, userCount)
	AutoSpeedLimit := int64(c.config.AutoSpeedLimitConfig.Limit)
	UpdatePeriodic := int64(c.config.UpdatePeriodic)
	limitThreshold := AutoSpeedLimit * 1000000 * UpdatePeriodic / 8
	limitedUsers := make([]api.UserInfo, 0, userCount)
	forceFullSweep := c.trafficSweepTick%fastTrafficFullSweepInterval == 0
	useFastScan := AutoSpeedLimit == 0 && !forceFullSweep
	if useFastScan {
		onlineUserKeys, onlineErr := c.GetOnlineUserKeys(c.Tag)
		if onlineErr != nil {
			c.logger.Print(onlineErr)
			useFastScan = false
		} else if len(onlineUserKeys) > 0 {
			userTraffic = make([]api.UserTraffic, 0, len(onlineUserKeys))
			upCounterList = make([]stats.Counter, 0, len(onlineUserKeys))
			downCounterList = make([]stats.Counter, 0, len(onlineUserKeys))
			seen := make(map[string]struct{}, len(onlineUserKeys))
			for _, userTag := range onlineUserKeys {
				if _, ok := seen[userTag]; ok {
					continue
				}
				seen[userTag] = struct{}{}

				up, down, upCounter, downCounter := c.getTraffic(userTag)
				if up == 0 && down == 0 {
					continue
				}
				if down > 0 {
					c.logger.Debugf("Traffic counted: tag=%s up=%d down=%d", userTag, up, down)
				}
				uid, email, ok := parseUserTag(userTag)
				if !ok {
					c.logger.Debugf("Invalid user tag skipped: %s", userTag)
					continue
				}
				userTraffic = append(userTraffic, api.UserTraffic{
					UID:      uid,
					Email:    email,
					Upload:   up,
					Download: down,
				})
				if upCounter != nil {
					upCounterList = append(upCounterList, upCounter)
				}
				if downCounter != nil {
					downCounterList = append(downCounterList, downCounter)
				}
			}
		}
	}

	if !useFastScan {
		for _, user := range *c.userList {
			uid := user.UID
			if limitInfo, ok := c.limitedUsers[uid]; ok {
				limitInfo.user = user
				c.limitedUsers[uid] = limitInfo
			}
			userTag := c.buildUserTag(&user)
			up, down, upCounter, downCounter := c.getTraffic(userTag)
			if down > 0 {
				c.logger.Debugf("Traffic counted: tag=%s up=%d down=%d", userTag, up, down)
			}
			if up > 0 || down > 0 {
				// Over speed users
				if AutoSpeedLimit > 0 {
					if down > limitThreshold || up > limitThreshold {
						if _, ok := c.limitedUsers[uid]; !ok {
							if c.config.AutoSpeedLimitConfig.WarnTimes == 0 {
								limitUser(c, user, &limitedUsers)
							} else {
								c.warnedUsers[uid] += 1
								if c.warnedUsers[uid] > c.config.AutoSpeedLimitConfig.WarnTimes {
									limitUser(c, user, &limitedUsers)
									delete(c.warnedUsers, uid)
								}
							}
						}
					} else {
						delete(c.warnedUsers, uid)
					}
				}
				userTraffic = append(userTraffic, api.UserTraffic{
					UID:      user.UID,
					Email:    user.Email,
					Upload:   up,
					Download: down})

				if upCounter != nil {
					upCounterList = append(upCounterList, upCounter)
				}
				if downCounter != nil {
					downCounterList = append(downCounterList, downCounter)
				}
			} else {
				delete(c.warnedUsers, uid)
			}
		}
	}
	if len(limitedUsers) > 0 {
		if err := c.UpdateInboundLimiter(c.Tag, &limitedUsers); err != nil {
			c.logger.Print(err)
		}
	}
	if len(userTraffic) > 0 {
		c.logger.Printf("Reporting %d user(s) traffic to panel; example: UID=%d up=%d down=%d", len(userTraffic), userTraffic[0].UID, userTraffic[0].Upload, userTraffic[0].Download)
		var err error // Define an empty error
		if !c.config.DisableUploadTraffic {
			err = c.apiClient.ReportUserTraffic(&userTraffic)
		}
		// If report traffic error, not clear the traffic
		if err != nil {
			c.logger.Print(err)
		} else {
			c.resetTraffic(&upCounterList, &downCounterList)
		}
	}

	// Report Online info
	if onlineDevice, err := c.GetOnlineDevice(c.Tag); err != nil {
		c.logger.Print(err)
	} else if len(*onlineDevice) > 0 {
		if err = c.apiClient.ReportNodeOnlineUsers(onlineDevice); err != nil {
			c.logger.Print(err)
		} else {
			c.logger.Printf("Report %d online users", len(*onlineDevice))
		}
	}

	// Report Illegal user
	if detectResult, err := c.GetDetectResult(c.Tag); err != nil {
		c.logger.Print(err)
	} else if len(*detectResult) > 0 {
		if err = c.apiClient.ReportIllegal(detectResult); err != nil {
			c.logger.Print(err)
		} else {
			c.logger.Printf("Report %d illegal behaviors", len(*detectResult))
		}

	}
	return nil
}

func (c *Controller) buildNodeTag() string {
	return fmt.Sprintf("%s_%s_%d", c.nodeInfo.NodeType, c.config.ListenIP, c.nodeInfo.Port)
}

// func (c *Controller) logPrefix() string {
// 	return fmt.Sprintf("[%s] %s(ID=%d)", c.clientInfo.APIHost, c.nodeInfo.NodeType, c.nodeInfo.NodeID)
// }

// Check Cert
func (c *Controller) certMonitor() error {
	if c.nodeInfo.EnableTLS && c.config.EnableREALITY == false {
		switch c.config.CertConfig.CertMode {
		case "dns", "http", "tls":
			lego, err := mylego.New(c.config.CertConfig)
			if err != nil {
				c.logger.Print(err)
			}
			// Xray-core supports the OcspStapling certification hot renew
			_, _, _, err = lego.RenewCert()
			if err != nil {
				c.logger.Print(err)
			}
		}
	}
	return nil
}
