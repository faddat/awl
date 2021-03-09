package peerlan

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ipfs/go-log/v2"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/peerlan/peerlan/api"
	"github.com/peerlan/peerlan/config"
	"github.com/peerlan/peerlan/p2p"
	"github.com/peerlan/peerlan/protocol"
	"github.com/peerlan/peerlan/ringbuffer"
	"github.com/peerlan/peerlan/service"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const (
	logBufSize = 100 * 1024
)

// @title Peerlan API
// @version 0.1
// @description Peerlan API

// @Host localhost:8639
// @BasePath /api/v0/

// TODO: move to main package (can't parse here)
//go:generate swag init --parseDependency
//go:generate rm -f docs/docs.go docs/swagger.json

type Application struct {
	LogBuffer *ringbuffer.RingBuffer
	logger    *log.ZapEventLogger
	Conf      *config.Config

	p2pServer  *p2p.P2p
	host       host.Host
	Api        *api.Handler
	P2pService *service.P2pService
	Forwarding *service.PortForwarding
	AuthStatus *service.AuthStatus
}

func New() *Application {
	return &Application{}
}

func (a *Application) Init(ctx context.Context) error {
	p2pSrv := p2p.NewP2p(ctx, a.Conf)
	host, err := p2pSrv.InitHost()
	if err != nil {
		return err
	}
	a.p2pServer = p2pSrv
	a.host = host

	privKey := host.Peerstore().PrivKey(host.ID())
	a.Conf.SetIdentity(privKey, host.ID())
	a.logger.Infof("Host created. We are: %s", host.ID().String())
	a.logger.Infof("Listen interfaces: %v", host.Addrs())

	err = p2pSrv.Bootstrap()
	if err != nil {
		return err
	}

	a.P2pService = service.NewP2p(p2pSrv, a.Conf)
	a.Forwarding = service.NewPortForwarding(a.P2pService, a.Conf)
	a.AuthStatus = service.NewAuthStatus(a.P2pService, a.Conf)

	host.SetStreamHandler(protocol.PortForwardingMethod, a.Forwarding.StreamHandler)
	host.SetStreamHandler(protocol.GetStatusMethod, a.AuthStatus.StatusStreamHandler)
	host.SetStreamHandler(protocol.AuthMethod, a.AuthStatus.AuthStreamHandler)

	handler := api.NewHandler(a.Conf, a.Forwarding, a.P2pService, a.AuthStatus, a.LogBuffer)
	a.Api = handler
	handler.SetupAPI()

	go a.P2pService.MaintainBackgroundConnections(a.Conf.P2pNode.ReconnectionIntervalSec)
	go a.AuthStatus.BackgroundRetryAuthRequests()
	go a.AuthStatus.BackgroundExchangeStatusInfo()

	return nil
}

func (a *Application) SetupLoggerAndConfig() *log.ZapEventLogger {
	// Config
	conf, err := config.LoadConfig()
	if err != nil {
		fmt.Printf("ERROR peerlan: failed to read config file, creating new one: %v\n", err)
		conf = config.NewConfig()
	}

	// Logger
	a.LogBuffer = ringbuffer.New(logBufSize)
	syncer := zapcore.NewMultiWriteSyncer(
		zapcore.Lock(zapcore.AddSync(os.Stdout)),
		zapcore.AddSync(a.LogBuffer),
	)

	encoderConfig := zap.NewDevelopmentEncoderConfig()
	encoderConfig.EncodeTime = func(t time.Time, enc zapcore.PrimitiveArrayEncoder) {
		enc.AppendString(t.Format("2006-01-02 15:04:05"))
	}
	consoleEncoder := zapcore.NewConsoleEncoder(encoderConfig)
	zapCore := zapcore.NewCore(consoleEncoder, syncer, zapcore.InfoLevel)

	lvl := conf.LogLevel()
	opts := []zap.Option{zap.AddStacktrace(zapcore.ErrorLevel)}
	if conf.DevMode() {
		opts = append(opts, zap.Development())
	}

	log.SetupLogging(zapCore, func(name string) zapcore.Level {
		switch {
		case strings.HasPrefix(name, "peerlan"):
			return lvl
		case name == "swarm2":
			// TODO: решить какой выставлять
			//return zapcore.InfoLevel // REMOVE
			return zapcore.ErrorLevel
		case name == "relay":
			return zapcore.WarnLevel
		case name == "connmgr":
			return zapcore.WarnLevel
		case name == "autonat":
			return zapcore.WarnLevel
		default:
			return zapcore.InfoLevel
		}
	},
		opts...,
	)

	a.logger = log.Logger("peerlan")
	a.Conf = conf

	return a.logger
}

func (a *Application) Close() {
	if a.Api != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		err := a.Api.Shutdown(ctx)
		if err != nil {
			a.logger.Errorf("closing api server: %v", err)
		}
	}
	if a.p2pServer != nil {
		err := a.p2pServer.Close()
		if err != nil {
			a.logger.Errorf("closing p2p server: %v", err)
		}
	}
	a.Conf.Save()
}