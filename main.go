// Copyright (c) 2020-2021 Doc.ai and/or its affiliates.
//
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at:
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/networkservicemesh/sdk/pkg/tools/opentracing"

	"github.com/networkservicemesh/sdk/pkg/networkservice/chains/nsmgrproxy"
	"github.com/networkservicemesh/sdk/pkg/tools/jaeger"
	"github.com/networkservicemesh/sdk/pkg/tools/spiffejwt"

	nested "github.com/antonfisher/nested-logrus-formatter"
	"github.com/kelseyhightower/envconfig"
	"github.com/sirupsen/logrus"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/networkservicemesh/sdk/pkg/tools/debug"
	"github.com/networkservicemesh/sdk/pkg/tools/grpcutils"
	"github.com/networkservicemesh/sdk/pkg/tools/log"
	"github.com/networkservicemesh/sdk/pkg/tools/log/logruslogger"
)

// Config is configuration for cmd-nsmgr-proxy
type Config struct {
	ListenOn         []url.URL     `default:"unix:///listen.on.socket" desc:"url to listen on." split_words:"true"`
	Name             string        `default:"nsmgr-proxy" desc:"Name of Network service manager proxy"`
	MaxTokenLifetime time.Duration `default:"24h" desc:"maximum lifetime of tokens" split_words:"true"`
}

func main() {
	// Setup context to catch signals
	ctx, cancel := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		// More Linux signals here
		syscall.SIGHUP,
		syscall.SIGTERM,
		syscall.SIGQUIT,
	)
	defer cancel()

	// Setup logging
	logrus.SetFormatter(&nested.Formatter{})
	ctx = log.WithFields(ctx, map[string]interface{}{"cmd": os.Args[0]})
	ctx = log.WithLog(ctx, logruslogger.New(ctx))

	// Debug self if necessary
	if err := debug.Self(); err != nil {
		log.FromContext(ctx).Infof("%s", err)
	}

	startTime := time.Now()

	// Get config from environment
	config := &Config{}
	if err := envconfig.Usage("nsmgr-proxy", config); err != nil {
		logrus.Fatal(err)
	}

	log.EnableTracing(true)
	closeJaeger := jaeger.InitJaeger(ctx, config.Name)
	defer func() {
		_ = closeJaeger.Close()
	}()
	if err := envconfig.Process("nsmgr-proxy", config); err != nil {
		logrus.Fatalf("error processing config from env: %+v", err)
	}

	log.FromContext(ctx).Infof("Config: %#v", config)

	// Get a X509Source
	source, err := workloadapi.NewX509Source(ctx)
	if err != nil {
		logrus.Fatalf("error getting x509 source: %+v", err)
	}
	svid, err := source.GetX509SVID()
	if err != nil {
		logrus.Fatalf("error getting x509 svid: %+v", err)
	}
	logrus.Infof("SVID: %q", svid.ID)

	tlsCreds := credentials.NewTLS(tlsconfig.MTLSServerConfig(source, source, tlsconfig.AuthorizeAny()))
	// Create GRPC Server and register services
	options := append(opentracing.WithTracing(), grpc.Creds(tlsCreds))
	server := grpc.NewServer(options...)

	dialOptions := append(opentracing.WithTracingDial(), grpc.WithBlock(), grpc.WithTransportCredentials(tlsCreds))
	nsmgrproxy.NewServer(
		ctx,
		spiffejwt.TokenGeneratorFunc(source, config.MaxTokenLifetime),
		nsmgrproxy.WithName(config.Name),
		nsmgrproxy.WithDialOptions(dialOptions...),
	).Register(server)

	for i := 0; i < len(config.ListenOn); i++ {
		srvErrCh := grpcutils.ListenAndServe(ctx, &config.ListenOn[i], server)
		exitOnErr(ctx, cancel, srvErrCh)
	}

	log.FromContext(ctx).Infof("Startup completed in %v", time.Since(startTime))
	<-ctx.Done()
}

func exitOnErr(ctx context.Context, cancel context.CancelFunc, errCh <-chan error) {
	// If we already have an error, log it and exit
	select {
	case err := <-errCh:
		log.FromContext(ctx).Fatal(err)
	default:
	}
	// Otherwise wait for an error in the background to log and cancel
	go func(ctx context.Context, errCh <-chan error) {
		err := <-errCh
		log.FromContext(ctx).Error(err)
		cancel()
	}(ctx, errCh)
}
