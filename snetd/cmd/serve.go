package cmd

import (
	"crypto/tls"
	"fmt"
	"github.com/singnet/snet-daemon/metrics"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/gorilla/handlers"
	"github.com/improbable-eng/grpc-web/go/grpcweb"
	"github.com/pkg/errors"
	"github.com/singnet/snet-daemon/blockchain"
	"github.com/singnet/snet-daemon/config"
	"github.com/singnet/snet-daemon/escrow"
	"github.com/singnet/snet-daemon/handler"
	"github.com/singnet/snet-daemon/handler/httphandler"
	"github.com/singnet/snet-daemon/logger"
	log "github.com/sirupsen/logrus"
	"github.com/soheilhy/cmux"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/acme/autocert"
	"golang.org/x/net/http2"
	"google.golang.org/grpc"
)

var corsOptions = []handlers.CORSOption{
	handlers.AllowedHeaders([]string{"Content-Type", "Snet-Job-Address", "Snet-Job-Signature"}),
}

var ServeCmd = &cobra.Command{
	Use: "serve",
	Run: func(cmd *cobra.Command, args []string) {
		var err error

		components := InitComponents(cmd)
		defer components.Close()

		etcdServer := components.EtcdServer()
		if etcdServer == nil {
			log.Info("Etcd server is disabled in the config file.")
		}

		err = logger.InitLogger(config.SubWithDefault(config.Vip(), config.LogKey))
		if err != nil {
			log.WithError(err).Fatal("Unable to initialize logger")
		}
		config.LogConfig()

		var d daemon
		d, err = newDaemon(components)
		if err != nil {
			log.WithError(err).Fatal("Unable to initialize daemon")
		}

		d.start()
		defer d.stop()

		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT)
		<-sigChan

		log.Debug("exiting")
	},
}

type daemon struct {
	autoSSLDomain string
	acmeListener  net.Listener
	grpcServer    *grpc.Server
	blockProc     blockchain.Processor
	lis           net.Listener
	sslCert       *tls.Certificate
	components    *Components
}

func newDaemon(components *Components) (daemon, error) {
	d := daemon{}

	if err := config.Validate(); err != nil {
		return d, err
	}

	// validate heartbeat configuration
	if err := metrics.ValidateHeartbeatConfig(); err != nil {
		return d, err
	}

	// validate alerts/notifications configuration
	if err := metrics.ValidateNotificationConfig(); err != nil {
		return d, err
	}

	d.components = components

	var err error
	port, err := deriveDaemonPort(config.GetString(config.DaemonEndPoint))
	if err != nil {
		return d, errors.Wrap(err, "error determining port")
	}
	log.WithField("port", port).Info("Starting listening port")

	d.lis, err = net.Listen("tcp", fmt.Sprintf("0.0.0.0:%+v", port))
	if err != nil {
		return d, errors.Wrap(err, "error listening")
	}

	d.autoSSLDomain = config.GetString(config.AutoSSLDomainKey)
	// In order to perform the LetsEncrypt (ACME) http-01 challenge-response, we need to bind
	// port 80 (privileged) to listen for the challenge.
	if d.autoSSLDomain != "" {
		d.acmeListener, err = net.Listen("tcp", ":80")
		if err != nil {
			return d, errors.Wrap(err, "unable to bind port 80 for automatic SSL verification")
		}
	}

	d.blockProc = *components.Blockchain()

	if sslKey := config.GetString(config.SSLKeyPathKey); sslKey != "" {
		cert, err := tls.LoadX509KeyPair(config.GetString(config.SSLCertPathKey), sslKey)
		if err != nil {
			return d, errors.Wrap(err, "unable to load specifiec SSL X509 keypair")
		}
		d.sslCert = &cert
	}

	return d, nil
}

func deriveDaemonPort(daemonEndpoint string) (string, error) {
	port := "8080"
	var err error = nil

	//There is a separate issue raised on standardizing the daemon end point format, #153, Daemon end point can also
	//be entered in the format localhost:8080 or 127.1.0.0:8080 ( as this is allowed while defining the service metadata )
	//For now strip http: or https: from the daemonEndPoint
	daemonEndpoint = strings.Replace(daemonEndpoint, "https://", "", -1)
	daemonEndpoint = strings.Replace(daemonEndpoint, "http://", "", -1)
	splitString := strings.Split(daemonEndpoint, ":")
	length := len(splitString)
	if length == 2 {
		port = splitString[len(splitString)-1]
		_, err = strconv.ParseInt(port, 0, 16)
		if err != nil {
			log.WithField("daemonEndPoint", daemonEndpoint).Error(err)
			err = fmt.Errorf("port number <%s> is not valid ,the daemon End point  %s", port, daemonEndpoint)
		}
	} else if length > 2 {
		err = fmt.Errorf("daemon end point should have a single ':' ,the daemon End point %s", daemonEndpoint)
	}
	return port, err
}

func (d daemon) start() {

	var tlsConfig *tls.Config

	if d.autoSSLDomain != "" {
		log.Debug("enabling automatic SSL support")
		certMgr := autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(d.autoSSLDomain),
			Cache:      autocert.DirCache(config.GetString(config.AutoSSLCacheDirKey)),
		}

		// This is the HTTP server that handles ACME challenge/response
		acmeSrv := http.Server{
			Handler: certMgr.HTTPHandler(nil),
		}
		go acmeSrv.Serve(d.acmeListener)

		tlsConfig = &tls.Config{
			GetCertificate: func(c *tls.ClientHelloInfo) (*tls.Certificate, error) {
				crt, err := certMgr.GetCertificate(c)
				if err != nil {
					log.WithError(err).Error("unable to fetch certificate")
				}
				return crt, err
			},
		}
	} else if d.sslCert != nil {
		log.Debug("enabling SSL support via X509 keypair")
		tlsConfig = &tls.Config{
			Certificates: []tls.Certificate{*d.sslCert},
		}
	}

	if tlsConfig != nil {
		// See: https://gist.github.com/soheilhy/bb272c000f1987f17063
		tlsConfig.NextProtos = []string{"http/1.1", http2.NextProtoTLS, "h2-14"}

		// Wrap underlying listener with a TLS listener
		d.lis = tls.NewListener(d.lis, tlsConfig)
	}

	if config.GetString(config.DaemonTypeKey) == "grpc" {
		// set the maximum that the server can receive to 4GB. It is set to for 4GB because of issue here https://github.com/grpc/grpc-go/issues/1590
		maxsizeOpt := grpc.MaxRecvMsgSize(4000000000)
		d.grpcServer = grpc.NewServer(
			grpc.UnknownServiceHandler(handler.NewGrpcHandler(d.components.ServiceMetaData())),
			grpc.StreamInterceptor(d.components.GrpcInterceptor()),
			maxsizeOpt,
		)
		escrow.RegisterPaymentChannelStateServiceServer(d.grpcServer, d.components.PaymentChannelStateService())
		escrow.RegisterProviderControlServiceServer(d.grpcServer,d.components.ProviderControlService())

		mux := cmux.New(d.lis)
		// Use "prefix" matching to support "application/grpc*" e.g. application/grpc+proto or +json
		// Use SendSettings for compatibility with Java gRPC clients:
		//   https://github.com/soheilhy/cmux#limitations
		grpcL := mux.MatchWithWriters(cmux.HTTP2MatchHeaderFieldPrefixSendSettings("content-type", "application/grpc"))
		httpL := mux.Match(cmux.HTTP1Fast())

		grpcWebServer := grpcweb.WrapServer(d.grpcServer, grpcweb.WithCorsForRegisteredEndpointsOnly(false))

		httpHandler := http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
			if grpcWebServer.IsGrpcWebRequest(req) || grpcWebServer.IsAcceptableGrpcCorsRequest(req) {
				grpcWebServer.ServeHTTP(resp, req)
			} else {
				if strings.Split(req.URL.Path, "/")[1] == "encoding" {
					resp.Header().Set("Access-Control-Allow-Origin", "*")
					fmt.Fprintln(resp, d.components.ServiceMetaData().GetWireEncoding())
				} else if strings.Split(req.URL.Path, "/")[1] == "heartbeat" {
					resp.Header().Set("Access-Control-Allow-Origin", "*")
					metrics.HeartbeatHandler(resp, req)
				} else {
					http.NotFound(resp, req)
				}
			}
		})

		log.Debug("starting daemon")

		go d.grpcServer.Serve(grpcL)
		go http.Serve(httpL, httpHandler)
		go mux.Serve()
	} else {
		log.Debug("starting simple HTTP daemon")

		go http.Serve(d.lis, handlers.CORS(corsOptions...)(httphandler.NewHTTPHandler(d.blockProc)))
	}

}

func (d daemon) stop() {

	if d.grpcServer != nil {
		d.grpcServer.Stop()
	}

	d.lis.Close()

	if d.acmeListener != nil {
		d.acmeListener.Close()
	}

	// TODO(aiden) add d.blockProc.StopLoop()
}
