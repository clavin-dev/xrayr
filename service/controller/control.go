package controller

import (
	"context"
	"fmt"

	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/inbound"
	"github.com/xtls/xray-core/features/outbound"
	"github.com/xtls/xray-core/features/stats"
	"github.com/xtls/xray-core/proxy"
	"github.com/xtls/xray-core/transport"

	"github.com/XrayR-project/XrayR/api"
	"github.com/XrayR-project/XrayR/common/limiter"
)

func (c *Controller) removeInbound(tag string) error {
	err := c.ibm.RemoveHandler(context.Background(), tag)
	return err
}

// statsOutboundWrapper wraps outbound.Handler to ensure user downlink traffic is counted.
type statsOutboundWrapper struct {
	outbound.Handler
	shouldDisableSplice func() bool
}

type trafficCounterPair struct {
	up   stats.Counter
	down stats.Counter
}

func (w *statsOutboundWrapper) Dispatch(ctx context.Context, link *transport.Link) {
	if w.shouldDisableSplice != nil && w.shouldDisableSplice() {
		// Disable kernel splice to avoid Vision/REALITY bypassing userland stats path.
		if sess := session.InboundFromContext(ctx); sess != nil {
			sess.CanSpliceCopy = 3
		}
	}
	w.Handler.Dispatch(ctx, link)
}

func (c *Controller) removeOutbound(tag string) error {
	err := c.obm.RemoveHandler(context.Background(), tag)
	return err
}

func (c *Controller) addInbound(config *core.InboundHandlerConfig) error {
	rawHandler, err := core.CreateObject(c.server, config)
	if err != nil {
		return err
	}
	handler, ok := rawHandler.(inbound.Handler)
	if !ok {
		return fmt.Errorf("not an InboundHandler: %s", err)
	}
	if err := c.ibm.AddHandler(context.Background(), handler); err != nil {
		return err
	}
	return nil
}

func (c *Controller) addOutbound(config *core.OutboundHandlerConfig) error {
	rawHandler, err := core.CreateObject(c.server, config)
	if err != nil {
		return err
	}
	handler, ok := rawHandler.(outbound.Handler)
	if !ok {
		return fmt.Errorf("not an InboundHandler: %s", err)
	}
	// Wrap outbound handler to ensure downlink stats are always counted (e.g., REALITY/VLESS cases)
	handler = &statsOutboundWrapper{
		Handler:             handler,
		shouldDisableSplice: c.dispatcher.ShouldDisableSplice,
	}
	if err := c.obm.AddHandler(context.Background(), handler); err != nil {
		return err
	}
	return nil
}

func (c *Controller) addUsers(users []*protocol.User, tag string) error {
	handler, err := c.ibm.GetHandler(context.Background(), tag)
	if err != nil {
		return fmt.Errorf("no such inbound tag: %s", err)
	}
	inboundInstance, ok := handler.(proxy.GetInbound)
	if !ok {
		return fmt.Errorf("handler %s has not implemented proxy.GetInbound", tag)
	}

	userManager, ok := inboundInstance.GetInbound().(proxy.UserManager)
	if !ok {
		return fmt.Errorf("handler %s has not implemented proxy.UserManager", tag)
	}
	for _, item := range users {
		if item == nil {
			continue
		}
		mUser, err := item.ToMemoryUser()
		if err != nil {
			return err
		}
		err = userManager.AddUser(context.Background(), mUser)
		if err != nil {
			return err
		}
		// Pre-register per-user traffic counters so core can increment them (downlink/uplink)
		uName := "user>>>" + mUser.Email + ">>>traffic>>>uplink"
		dName := "user>>>" + mUser.Email + ">>>traffic>>>downlink"
		if _, _ = stats.GetOrRegisterCounter(c.stm, uName); true {
			// no-op
		}
		if _, _ = stats.GetOrRegisterCounter(c.stm, dName); true {
			// no-op
		}
	}
	return nil
}

func (c *Controller) removeUsers(users []string, tag string) error {
	handler, err := c.ibm.GetHandler(context.Background(), tag)
	if err != nil {
		return fmt.Errorf("no such inbound tag: %s", err)
	}
	inboundInstance, ok := handler.(proxy.GetInbound)
	if !ok {
		return fmt.Errorf("handler %s is not implement proxy.GetInbound", tag)
	}

	userManager, ok := inboundInstance.GetInbound().(proxy.UserManager)
	if !ok {
		return fmt.Errorf("handler %s is not implement proxy.UserManager", err)
	}
	for _, email := range users {
		err = userManager.RemoveUser(context.Background(), email)
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) getTraffic(email string) (up int64, down int64, upCounter stats.Counter, downCounter stats.Counter) {
	if cached, ok := c.trafficCounterCache.Load(email); ok {
		if pair, ok := cached.(trafficCounterPair); ok {
			upCounter = pair.up
			downCounter = pair.down
		}
	}
	if upCounter == nil || downCounter == nil {
		upName := "user>>>" + email + ">>>traffic>>>uplink"
		downName := "user>>>" + email + ">>>traffic>>>downlink"
		upCounter = c.stm.GetCounter(upName)
		downCounter = c.stm.GetCounter(downName)
		c.trafficCounterCache.Store(email, trafficCounterPair{
			up:   upCounter,
			down: downCounter,
		})
	}
	if upCounter != nil {
		up = upCounter.Value()
		if up == 0 {
			upCounter = nil
		}
	} else {
		upCounter = nil
	}
	if downCounter != nil {
		down = downCounter.Value()
		if down == 0 {
			downCounter = nil
		}
	} else {
		downCounter = nil
	}
	return up, down, upCounter, downCounter
}

func (c *Controller) resetTraffic(upCounterList *[]stats.Counter, downCounterList *[]stats.Counter) {
	for _, upCounter := range *upCounterList {
		upCounter.Set(0)
	}
	for _, downCounter := range *downCounterList {
		downCounter.Set(0)
	}
}

func (c *Controller) AddInboundLimiter(tag string, nodeSpeedLimit uint64, userList *[]api.UserInfo, globalDeviceLimitConfig *limiter.GlobalDeviceLimitConfig) error {
	err := c.dispatcher.Limiter.AddInboundLimiter(tag, nodeSpeedLimit, userList, globalDeviceLimitConfig)
	return err
}

func (c *Controller) UpdateInboundLimiter(tag string, updatedUserList *[]api.UserInfo) error {
	err := c.dispatcher.Limiter.UpdateInboundLimiter(tag, updatedUserList)
	return err
}

func (c *Controller) DeleteInboundLimiter(tag string) error {
	err := c.dispatcher.Limiter.DeleteInboundLimiter(tag)
	return err
}

func (c *Controller) GetOnlineDevice(tag string) (*[]api.OnlineUser, error) {
	return c.dispatcher.Limiter.GetOnlineDevice(tag)
}

func (c *Controller) GetOnlineUserKeys(tag string) ([]string, error) {
	return c.dispatcher.Limiter.GetOnlineUserKeys(tag)
}

func (c *Controller) DropUserTrafficCounterCache(userTag string) {
	c.dispatcher.DropUserTrafficCounterCache(userTag)
}

func (c *Controller) ResetUserTrafficCounterCache() {
	c.dispatcher.ResetUserTrafficCounterCache()
}

func (c *Controller) UpdateRule(tag string, newRuleList []api.DetectRule) error {
	err := c.dispatcher.RuleManager.UpdateRule(tag, newRuleList)
	return err
}

func (c *Controller) GetDetectResult(tag string) (*[]api.DetectResult, error) {
	return c.dispatcher.RuleManager.GetDetectResult(tag)
}
