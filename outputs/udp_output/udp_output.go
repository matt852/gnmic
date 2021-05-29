package udp_output

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"time"

	"github.com/karimra/gnmic/formatters"
	"github.com/karimra/gnmic/outputs"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"
)

const (
	defaultRetryTimer = 2 * time.Second
	loggingPrefix     = "[udp_output] "
)

func init() {
	outputs.Register("udp", func() outputs.Output {
		return &UDPSock{
			Cfg:    &Config{},
			logger: log.New(ioutil.Discard, loggingPrefix, log.LstdFlags|log.Lmicroseconds),
		}
	})
}

type UDPSock struct {
	Cfg *Config

	conn     *net.UDPConn
	cancelFn context.CancelFunc
	buffer   chan []byte
	limiter  *time.Ticker
	logger   *log.Logger
	mo       *formatters.MarshalOptions
	evps     []formatters.EventProcessor
}

type Config struct {
	Address           string        `mapstructure:"address,omitempty"` // ip:port
	Rate              time.Duration `mapstructure:"rate,omitempty"`
	BufferSize        uint          `mapstructure:"buffer-size,omitempty"`
	Format            string        `mapstructure:"format,omitempty"`
	OverrideTimestamp bool          `mapstructure:"override-ts,omitempty"`
	RetryInterval     time.Duration `mapstructure:"retry-interval,omitempty"`
	EnableMetrics     bool          `mapstructure:"enable-metrics,omitempty"`
	EventProcessors   []string      `mapstructure:"event-processors,omitempty"`
}

func (u *UDPSock) SetLogger(logger *log.Logger) {
	if logger != nil && u.logger != nil {
		u.logger.SetOutput(logger.Writer())
		u.logger.SetFlags(logger.Flags())
	}
}

func (u *UDPSock) SetEventProcessors(ps map[string]map[string]interface{}, logger *log.Logger, tcs map[string]interface{}) {
	for _, epName := range u.Cfg.EventProcessors {
		if epCfg, ok := ps[epName]; ok {
			epType := ""
			for k := range epCfg {
				epType = k
				break
			}
			if in, ok := formatters.EventProcessors[epType]; ok {
				ep := in()
				err := ep.Init(epCfg[epType], formatters.WithLogger(logger), formatters.WithTargets(tcs))
				if err != nil {
					u.logger.Printf("failed initializing event processor '%s' of type='%s': %v", epName, epType, err)
					continue
				}
				u.evps = append(u.evps, ep)
				u.logger.Printf("added event processor '%s' of type=%s to udp output", epName, epType)
			}
		}
	}
}

func (u *UDPSock) Init(ctx context.Context, name string, cfg map[string]interface{}, opts ...outputs.Option) error {
	err := outputs.DecodeConfig(cfg, u.Cfg)
	if err != nil {
		return err
	}
	for _, opt := range opts {
		opt(u)
	}
	_, _, err = net.SplitHostPort(u.Cfg.Address)
	if err != nil {
		return fmt.Errorf("wrong address format: %v", err)
	}
	if u.Cfg.RetryInterval == 0 {
		u.Cfg.RetryInterval = defaultRetryTimer
	}

	u.buffer = make(chan []byte, u.Cfg.BufferSize)
	if u.Cfg.Rate > 0 {
		u.limiter = time.NewTicker(u.Cfg.Rate)
	}
	go func() {
		<-ctx.Done()
		u.Close()
	}()
	ctx, u.cancelFn = context.WithCancel(ctx)
	u.mo = &formatters.MarshalOptions{
		Format:     u.Cfg.Format,
		OverrideTS: u.Cfg.OverrideTimestamp,
	}
	go u.start(ctx)
	return nil
}

func (u *UDPSock) Write(ctx context.Context, m proto.Message, meta outputs.Meta) {
	if m == nil {
		return
	}
	b, err := u.mo.Marshal(m, meta, u.evps...)
	if err != nil {
		u.logger.Printf("failed marshaling proto msg: %v", err)
		return
	}
	u.buffer <- b
}

func (u *UDPSock) WriteEvent(ctx context.Context, ev *formatters.EventMsg) {}

func (u *UDPSock) Close() error {
	u.cancelFn()
	if u.limiter != nil {
		u.limiter.Stop()
	}
	return nil
}

func (u *UDPSock) RegisterMetrics(reg *prometheus.Registry) {}

func (u *UDPSock) String() string {
	b, err := json.Marshal(u)
	if err != nil {
		return ""
	}
	return string(b)
}

func (u *UDPSock) start(ctx context.Context) {
	var udpAddr *net.UDPAddr
	var err error
	defer u.Close()
DIAL:
	if ctx.Err() != nil {
		u.logger.Printf("context error: %v", ctx.Err())
		return
	}
	udpAddr, err = net.ResolveUDPAddr("udp", u.Cfg.Address)
	if err != nil {
		u.logger.Printf("failed to dial udp: %v", err)
		time.Sleep(u.Cfg.RetryInterval)
		goto DIAL
	}
	u.conn, err = net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		u.logger.Printf("failed to dial udp: %v", err)
		time.Sleep(u.Cfg.RetryInterval)
		goto DIAL
	}
	for {
		select {
		case <-ctx.Done():
			return
		case b := <-u.buffer:
			if u.limiter != nil {
				<-u.limiter.C
			}
			_, err = u.conn.Write(b)
			if err != nil {
				u.logger.Printf("failed sending udp bytes: %v", err)
				time.Sleep(u.Cfg.RetryInterval)
				goto DIAL
			}
		}
	}
}

func (u *UDPSock) SetName(name string)        {}
func (u *UDPSock) SetClusterName(name string) {}
