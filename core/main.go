package core

import (
	"context"
	"sync"

	"github.com/alireza0/s-ui/util/common"

	sb "github.com/sagernet/sing-box"
	_ "github.com/sagernet/sing-box/experimental/clashapi"
	_ "github.com/sagernet/sing-box/experimental/v2rayapi"
	"github.com/sagernet/sing-box/option"
	_ "github.com/sagernet/sing-box/transport/v2rayquic"
)

// Core owns the running sing-box instance.
//
// Everything mutable lives behind mu. The managers this used to keep in
// package-level vars (inbound_manager, router, factory, ...) were copies of
// fields the Box already holds, written by Start and read by the endpoint
// methods without synchronisation; they are now read off the Box itself, so a
// caller cannot mix managers from one box with an instance from another.
//
// mu is an RWMutex because IsRunning is called by the panel's status poll on
// every request, and must not queue behind a start.
//
// Lock ordering: service.startCoreMu (outer) is always taken before Core.mu
// (inner). Nothing in this package calls into package service, so that order
// holds by construction.
type Core struct {
	mu        sync.RWMutex
	isRunning bool
	instance  *Box

	// ctx carries the protocol registries. It is built once in NewCore and
	// never reassigned, so it needs no lock. Start used to wrap it on every
	// run (globalCtx = service.ContextWith(globalCtx, c)) -- nothing ever read
	// that value back, and it added a context layer per restart.
	ctx context.Context
}

func NewCore() *Core {
	ctx := sb.Context(context.Background(), InboundRegistry(), OutboundRegistry(),
		EndpointRegistry(), DNSTransportRegistry(), ServiceRegistry(), CertificateProviderRegistry())
	return &Core{ctx: ctx}
}

func (c *Core) GetCtx() context.Context {
	return c.ctx
}

func (c *Core) GetInstance() *Box {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.instance
}

func (c *Core) IsRunning() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.isRunning
}

// running returns the live box, or an error when the core is stopped. One
// RLock gives the caller a consistent view: it cannot observe isRunning as true
// and then find instance already nil, which is what made the old
// check-then-use pattern panic when a stop landed in between.
func (c *Core) running() (*Box, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.isRunning || c.instance == nil {
		return nil, common.NewError("sing-box is not running")
	}
	return c.instance, nil
}

// Start builds a box from the config and publishes it.
//
// The build and the start happen outside the lock on purpose: NewBox
// constructs every inbound, outbound and endpoint, and Start binds their
// listeners, which takes seconds. Holding mu across that would block every
// status poll for the duration. Only the publish is locked, so a half-built
// box is never visible -- a failed start is closed and discarded without ever
// being assigned.
func (c *Core) Start(sbConfig []byte) error {
	var opt option.Options
	// A malformed config used to be logged and then used anyway, which left
	// an empty option set: the box started with zero inbounds and reported
	// itself healthy, so the watchdog never retried and no client could
	// connect.
	if err := opt.UnmarshalJSONContext(c.ctx, sbConfig); err != nil {
		return common.NewErrorf("unmarshal config: %v", err)
	}

	instance, err := NewBox(Options{
		Context: c.ctx,
		Options: opt,
	})
	if err != nil {
		return err
	}

	if err = instance.Start(); err != nil {
		_ = instance.Close()
		return err
	}

	c.mu.Lock()
	c.instance = instance
	c.isRunning = true
	c.mu.Unlock()
	return nil
}

// Stop detaches the instance under the lock and closes it outside, since
// Box.Close shuts down ten manager subsystems in sequence.
func (c *Core) Stop() error {
	c.mu.Lock()
	instance := c.instance
	c.instance = nil
	c.isRunning = false
	c.mu.Unlock()

	if instance == nil {
		return nil
	}
	return instance.Close()
}
