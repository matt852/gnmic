// © 2022 Nokia.
//
// This code is a Contribution to the gNMIc project (“Work”) made under the Google Software Grant and Corporate Contributor License Agreement (“CLA”) and governed by the Apache License 2.0.
// No other rights or licenses in or to any of Nokia’s intellectual property are granted for any other purpose.
// This code is provided on an “as is” basis without any warranties of any kind.
//
// SPDX-License-Identifier: Apache-2.0

package cache

import (
	"context"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	ocCache "github.com/openconfig/gnmi/cache"
	"github.com/openconfig/gnmi/ctree"
	"github.com/openconfig/gnmi/match"
	"github.com/openconfig/gnmi/path"
	"github.com/openconfig/gnmi/proto/gnmi"
	"github.com/openconfig/gnmi/subscribe"
	gpath "github.com/openconfig/gnmic/pkg/path"
	"github.com/openconfig/gnmic/pkg/utils"
	"google.golang.org/protobuf/proto"
)

const (
	loggingPrefixOC = "[cache:oc] "
	defaultTimeout  = 10 * time.Second
)

type gnmiCache struct {
	m      *sync.Mutex
	caches map[string]*subCache
	// match  *match.Match

	logger     *log.Logger
	expiration time.Duration
	debug      bool
}

type subCache struct {
	c     *ocCache.Cache
	match *match.Match
}

func (gc *gnmiCache) loadConfig(gcc *Config) {
	gc.expiration = gcc.Expiration
	gc.logger = log.New(io.Discard, loggingPrefixOC, utils.DefaultLoggingFlags)
	gc.debug = gcc.Debug
}

func newGNMICache(cfg *Config, loggingPrefix string, opts ...Option) *gnmiCache {
	if cfg == nil {
		cfg = new(Config)
	}
	gc := &gnmiCache{
		m: new(sync.Mutex),
		// match:  match.New(),
		caches: make(map[string]*subCache),
	}
	cfg.setDefaults()

	gc.loadConfig(cfg)
	for _, opt := range opts {
		opt(gc)
	}
	if gc.logger != nil {
		if loggingPrefix == "" {
			loggingPrefix = "oc"
		}
		gc.logger.SetPrefix(loggingPrefixOC)
	}
	return gc
}

func (gc *subCache) update(n *ctree.Leaf) {
	switch v := n.Value().(type) {
	case *gnmi.Notification:
		pathElems := path.ToStrings(v.GetPrefix(), true)
		subscribe.UpdateNotification(gc.match, n, v, pathElems)
	default:
		// gc.logger.Printf("unexpected update type: %T", v)
	}
}

func (gc *gnmiCache) SetLogger(logger *log.Logger) {
	if logger != nil && gc.logger != nil {
		gc.logger.SetOutput(logger.Writer())
		gc.logger.SetFlags(logger.Flags())
	}
}

func (gc *gnmiCache) Write(ctx context.Context, measName string, m proto.Message) {
	var err error
	switch rsp := m.ProtoReflect().Interface().(type) {
	case *gnmi.SubscribeResponse:
		switch rsp := rsp.GetResponse().(type) {
		case *gnmi.SubscribeResponse_Update:
			target := rsp.Update.GetPrefix().GetTarget()
			if target == "" {
				gc.logger.Printf("subscription=%q: response missing target: %v", measName, rsp)
				return
			}

			// if the update does not have a prefix path,
			// check that each update has a path.
			if len(rsp.Update.GetPrefix().GetElem()) == 0 {
				for _, upd := range rsp.Update.GetUpdate() {
					if len(upd.GetPath().GetElem()) == 0 {
						gc.logger.Printf("write fail: received an update with en empty path: %v", upd)
						return
					}
				}
			}
			gc.m.Lock()
			sCache, ok := gc.caches[measName]
			if !ok {
				sCache = &subCache{
					c:     ocCache.New(nil),
					match: match.New(),
				}
				sCache.c.SetClient(sCache.update)
				sCache.c.Add(target)
				gc.logger.Printf("target %q added to local cache %q", target, measName)
				gc.caches[measName] = sCache
			}
			if !sCache.c.HasTarget(target) {
				sCache.c.Add(target)
				gc.logger.Printf("target %q added to local cache %q", target, measName)
			}
			gc.m.Unlock()
			// do not write updates with nil values to cache.
			notif := &gnmi.Notification{
				Timestamp: rsp.Update.GetTimestamp(),
				Prefix:    rsp.Update.GetPrefix(),
				Update:    make([]*gnmi.Update, 0, len(rsp.Update.GetUpdate())),
				Delete:    rsp.Update.GetDelete(),
				Atomic:    rsp.Update.GetAtomic(),
			}
			for _, upd := range rsp.Update.GetUpdate() {
				if upd.Val == nil {
					continue
				}
				notif.Update = append(notif.Update, upd)
			}
			if len(notif.Update) == 0 && len(notif.Delete) == 0 {
				return
			}
			err = sCache.c.GnmiUpdate(notif)
			if err != nil {
				gc.logger.Printf("failed to update gNMI cache: %v", err)
				return
			}
			return
		}
	}
}

func (gc *gnmiCache) ReadAll() (map[string][]*gnmi.Notification, error) {
	return gc.read("", "*", nil), nil
}

func (gc *gnmiCache) Read(sub, target string, p *gnmi.Path) (map[string][]*gnmi.Notification, error) {
	return gc.read(sub, target, p), nil
}

func (gc *gnmiCache) Subscribe(ctx context.Context, ro *ReadOpts) chan *Notification {
	if ro == nil {
		ro = new(ReadOpts)
	}

	ro.setDefaults()
	ch := make(chan *Notification)
	go gc.subscribe(ctx, ro, ch)

	return ch
}

func (gc *gnmiCache) subscribe(ctx context.Context, ro *ReadOpts, ch chan *Notification) {
	defer close(ch)
	switch ro.Mode {
	case ReadMode_Once:
		gc.handleSingleQuery(ctx, ro, ch)
	case ReadMode_StreamOnChange: // default:
		ro.SuppressRedundant = false
		gc.handleOnChangeQuery(ctx, ro, ch)
	case ReadMode_StreamSample:
		gc.handleSampledQuery(ctx, ro, ch)
	}
}

func (gc *gnmiCache) handleSingleQuery(ctx context.Context, ro *ReadOpts, ch chan *Notification) {
	if gc.debug {
		gc.logger.Printf("running single query for target %q", ro.Target)
	}

	caches := gc.getCaches(ro.Subscription)

	if gc.debug {
		gc.logger.Printf("single query got %d caches", len(caches))
	}
	wg := new(sync.WaitGroup)
	wg.Add(len(caches))

	for name, c := range caches {
		go func(name string, c *subCache) {
			defer wg.Done()
			if !c.c.HasTarget(ro.Target) {
				if gc.debug {
					gc.logger.Printf("subscription-cache %q doesn't have target: %q", name, ro.Target)
				}
				return
			}
			for _, p := range ro.Paths {
				fp, err := path.CompletePath(p, nil)
				if err != nil {
					gc.logger.Printf("failed to generate CompletePath from %v", p)
					ch <- &Notification{Name: name, Err: err}
					return
				}
				err = c.c.Query(ro.Target, fp,
					func(_ []string, l *ctree.Leaf, _ interface{}) error {
						if err != nil {
							return err
						}
						switch gl := l.Value().(type) {
						case *gnmi.Notification:
							if ro.OverrideTS {
								// override timestamp
								gl = proto.Clone(gl).(*gnmi.Notification)
								gl.Timestamp = time.Now().UnixNano()
							}
							//no suppress redundant, send to channel and return
							if !ro.SuppressRedundant {
								ch <- &Notification{Name: name, Notification: gl}
								return nil
							}
							// suppress redundant part
							if ro.lastSent == nil {
								ro.lastSent = make(map[string]*gnmi.TypedValue)
								ro.m = new(sync.RWMutex)
							}

							prefix := gpath.GnmiPathToXPath(gl.GetPrefix(), true)
							target := gl.GetPrefix().GetTarget()
							for _, upd := range gl.GetUpdate() {
								p := gpath.GnmiPathToXPath(upd.GetPath(), true)
								valXPath := strings.Join([]string{target, prefix, p}, "/")
								ro.m.RLock()
								sv, ok := ro.lastSent[valXPath]
								ro.m.RUnlock()
								if !ok || !proto.Equal(sv, upd.Val) {
									ch <- &Notification{
										Name: name,
										Notification: &gnmi.Notification{
											Timestamp: gl.GetTimestamp(),
											Prefix:    gl.GetPrefix(),
											Update:    []*gnmi.Update{upd},
										},
									}
									ro.m.Lock()
									ro.lastSent[valXPath] = upd.Val
									ro.m.Unlock()
								}
							}

							if gl.GetDelete() != nil {
								ch <- &Notification{
									Name: name,
									Notification: &gnmi.Notification{
										Timestamp: gl.GetTimestamp(),
										Prefix:    gl.GetPrefix(),
										Delete:    gl.GetDelete(),
									},
								}
							}
							return nil
						}
						return nil
					})
				if err != nil {
					gc.logger.Printf("target %q failed internal cache query: %v", ro.Target, err)
					ch <- &Notification{Name: name, Err: err}
					return
				}
			}
		}(name, c)
	}
	wg.Wait()
}

func (gc *gnmiCache) handleSampledQuery(ctx context.Context, ro *ReadOpts, ch chan *Notification) {
	if !ro.UpdatesOnly {
		gc.handleSingleQuery(ctx, ro, ch)
	}

	ticker := time.NewTicker(ro.SampleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			gc.logger.Printf("periodic query to target %q stopped: %v", ro.Target, ctx.Err())
			return
		case <-ticker.C:
			gc.handleSingleQuery(ctx, ro, ch)
		}
	}
}

func (gc *gnmiCache) handleOnChangeQuery(ctx context.Context, ro *ReadOpts, ch chan *Notification) {
	caches := gc.getCaches(ro.Subscription)
	numCaches := len(caches)
	gc.logger.Printf("on-change query got %d cache(s)", numCaches)

	wg := new(sync.WaitGroup)
	wg.Add(numCaches)

	for name, c := range caches {
		go func(name string, c *subCache) {
			defer wg.Done()
			if !c.c.HasTarget(ro.Target) {
				if gc.debug {
					gc.logger.Printf("subscription-cache %q doesn't have target: %q", name, ro.Target)
				}
				return
			}
			for _, p := range ro.Paths {
				cp, err := path.CompletePath(p, nil)
				if err != nil {
					gc.logger.Printf("failed to generate CompletePath from %v", p)
					ch <- &Notification{Name: name, Err: err}
					return
				}
				// handle updates only
				if !ro.UpdatesOnly {
					err = c.c.Query(ro.Target, cp,
						func(_ []string, l *ctree.Leaf, _ interface{}) error {
							switch gl := l.Value().(type) {
							case *gnmi.Notification:
								ch <- &Notification{Name: name, Notification: gl}
							}
							return nil
						})
					if err != nil {
						gc.logger.Printf("failed to run cache query for target %q and path %q: %v", ro.Target, cp, err)
						ch <- &Notification{Name: name, Err: err}
						return
					}
				}
				// main on-change subscription
				fp := make([]string, 0, len(cp)+1)
				fp = append(fp, ro.Target)
				fp = append(fp, cp...)
				// set callback
				mc := &matchClient{name: name, ch: ch}
				remove := c.match.AddQuery(fp, mc)
				defer remove()

				// handle on-change heartbeat
				if ro.HeartbeatInterval > 0 {
					// run a sampled query using heartbeat interval as sample interval
					gc.handleSampledQuery(ctx, &ReadOpts{
						Subscription:   ro.Subscription,
						Target:         ro.Target,
						Paths:          ro.Paths,
						Mode:           ReadMode_StreamSample,
						SampleInterval: ro.HeartbeatInterval,
						OverrideTS:     ro.OverrideTS,
					}, ch)
				}
			}

			for range ctx.Done() {
			}
		}(name, c)
	}
	wg.Wait()
}

func (gc *gnmiCache) Stop() {}

func (gc *gnmiCache) read(sub, target string, p *gnmi.Path) map[string][]*gnmi.Notification {
	notificationChan := make(chan *Notification)
	notifications := make(map[string][]*gnmi.Notification, 0)
	doneCh := make(chan struct{})
	// this go routine will collect all the notifications
	// from the cache queries
	go func() {
		for nn := range notificationChan {
			if _, ok := notifications[nn.Name]; !ok {
				notifications[nn.Name] = make([]*gnmi.Notification, 0)
			}
			notifications[nn.Name] = append(notifications[nn.Name], nn.Notification)
		}
		close(doneCh)
	}()
	if sub == "*" {
		sub = ""
	}
	now := time.Now()
	wg := new(sync.WaitGroup)
	caches := gc.getCaches(sub)
	wg.Add(len(caches))

	for name, c := range caches {
		go func(c *subCache, name string) {
			defer wg.Done()
			cp, err := path.CompletePath(p, nil)
			if err != nil {
				gc.logger.Printf("failed to generate CompletePath from %v", p)
				return
			}
			err = c.c.Query(target, cp,
				func(_ []string, _ *ctree.Leaf, v interface{}) error {
					if err != nil {
						return err
					}
					switch notif := v.(type) {
					case *gnmi.Notification:
						if gc.expiration > 0 &&
							time.Unix(0, notif.Timestamp).Before(now.Add(time.Duration(-gc.expiration))) {
							return nil
						}
						notificationChan <- &Notification{
							Name:         name,
							Notification: notif,
						}
					}
					return nil
				})
			if err != nil {
				gc.logger.Printf("failed cache query:%v", err)
				return
			}
		}(c, name)
	}
	wg.Wait()
	close(notificationChan)
	// wait for notifications to be appended to the array
	<-doneCh
	return notifications
}

func (gc *gnmiCache) getCaches(names ...string) map[string]*subCache {
	gc.m.Lock()
	defer gc.m.Unlock()

	caches := make(map[string]*subCache)
	numCaches := len(names)
	if numCaches == 0 || (numCaches == 1 && names[0] == "") {
		for n, c := range gc.caches {
			caches[n] = c
		}
		return caches
	}
	for _, n := range names {
		if c, ok := gc.caches[n]; ok {
			caches[n] = c
		}
	}
	return caches
}

func (gc *gnmiCache) DeleteTarget(name string) {
	caches := gc.getCaches()
	for _, c := range caches {
		c.c.Remove(name)
	}
}

// match client
type matchClient struct {
	name string
	ch   chan *Notification
}

func (m *matchClient) Update(n interface{}) {
	switch n := n.(type) {
	case *ctree.Leaf:
		switch v := n.Value().(type) {
		case *gnmi.Notification:
			m.ch <- &Notification{
				Name:         m.name,
				Notification: v,
			}
		}
	}
}
