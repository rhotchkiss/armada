package client

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	grpc_retry "github.com/grpc-ecosystem/go-grpc-middleware/retry"
	"github.com/pkg/errors"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	"github.com/G-Research/armada/internal/common"
	"github.com/G-Research/armada/pkg/client/auth/exec"
	"github.com/G-Research/armada/pkg/client/auth/kerberos"
	"github.com/G-Research/armada/pkg/client/auth/oidc"
)

type ApiConnectionDetails struct {
	ArmadaUrl     string
	ArmadaRestUrl string
	// After a duration of this time, if the client doesn't see any activity it
	// pings the server to see if the transport is still alive.
	// If set below 10s, a minimum value of 10s is used instead.
	// The default value is infinity.
	GrpcKeepAliveTime time.Duration
	// After having pinged for keepalive check, the client waits for a duration
	// of Timeout and if no activity is seen even after that the connection is
	// closed.
	GrpcKeepAliveTimeout time.Duration
	// Authentication options.
	BasicAuth                   common.LoginCredentials
	OpenIdAuth                  oidc.PKCEDetails
	OpenIdDeviceAuth            oidc.DeviceDetails
	OpenIdPasswordAuth          oidc.ClientPasswordDetails
	OpenIdClientCredentialsAuth oidc.ClientCredentialsDetails
	OpenIdKubernetesAuth        oidc.KubernetesDetails
	KerberosAuth                kerberos.ClientConfig
	ForceNoTls                  bool
	ExecAuth                    exec.CommandDetails
}

type ConnectionDetails func() *ApiConnectionDetails

func CreateApiConnection(config *ApiConnectionDetails, additionalDialOptions ...grpc.DialOption) (*grpc.ClientConn, error) {
	return CreateApiConnectionWithCallOptions(config, []grpc.CallOption{}, additionalDialOptions...)
}

func CreateApiConnectionWithCallOptions(
	config *ApiConnectionDetails,
	additionalDefaultCallOptions []grpc.CallOption,
	additionalDialOptions ...grpc.DialOption) (*grpc.ClientConn, error) {

	retryOpts := []grpc_retry.CallOption{
		grpc_retry.WithBackoff(grpc_retry.BackoffExponential(1 * time.Second)),
		grpc_retry.WithMax(5),
	}

	callOptions := append(additionalDefaultCallOptions, grpc.WaitForReady(true))
	defaultCallOptions := grpc.WithDefaultCallOptions(callOptions...)
	unuaryInterceptors := grpc.WithChainUnaryInterceptor(grpc_retry.UnaryClientInterceptor(retryOpts...))
	streamInterceptors := grpc.WithChainStreamInterceptor(grpc_retry.StreamClientInterceptor(retryOpts...))
	dialOpts := append(additionalDialOptions,
		defaultCallOptions,
		unuaryInterceptors,
		streamInterceptors,
		transportCredentials(config),
	)
	// gRPC keepalive options.
	if config.GrpcKeepAliveTime > 0 || config.GrpcKeepAliveTimeout > 0 {
		keepAliveOptions := grpc.WithKeepaliveParams(
			keepalive.ClientParameters{
				Time:    config.GrpcKeepAliveTime,
				Timeout: config.GrpcKeepAliveTimeout,
			},
		)
		dialOpts = append(dialOpts, keepAliveOptions)
	}

	creds, err := perRpcCredentials(config)
	if err != nil {
		return nil, err
	}
	if creds != nil {
		dialOpts = append(dialOpts, grpc.WithPerRPCCredentials(creds))
	}

	return grpc.Dial(config.ArmadaUrl, dialOpts...)
}

func perRpcCredentials(config *ApiConnectionDetails) (credentials.PerRPCCredentials, error) {
	if config.BasicAuth.Username != "" {
		return &config.BasicAuth, nil

	} else if config.OpenIdAuth.ProviderUrl != "" {
		return oidc.AuthenticatePkce(config.OpenIdAuth)

	} else if config.OpenIdDeviceAuth.ProviderUrl != "" {
		return oidc.AuthenticateDevice(config.OpenIdDeviceAuth)

	} else if config.OpenIdPasswordAuth.ProviderUrl != "" {
		return oidc.AuthenticateWithPassword(config.OpenIdPasswordAuth)

	} else if config.OpenIdClientCredentialsAuth.ProviderUrl != "" {
		return oidc.AuthenticateWithClientCredentials(config.OpenIdClientCredentialsAuth)

	} else if config.OpenIdKubernetesAuth.ProviderUrl != "" {
		return oidc.AuthenticateKubernetes(config.OpenIdKubernetesAuth)
	} else if config.KerberosAuth.Enabled {
		return kerberos.NewSPNEGOCredentials(config.ArmadaUrl, config.KerberosAuth)
	} else if config.ExecAuth.Cmd != "" {
		return exec.NewAuthenticator(config.ExecAuth), nil
	}
	return nil, nil
}

func transportCredentials(config *ApiConnectionDetails) grpc.DialOption {
	if !config.ForceNoTls && !strings.Contains(config.ArmadaUrl, "localhost") {
		return grpc.WithTransportCredentials(credentials.NewClientTLSFromCert(nil, ""))
	}
	return grpc.WithTransportCredentials(insecure.NewCredentials())
}

// ArmadaHealthCheck calls Armada Server /health endpoint.
//
// Returns true if response status code is in range [200-399], otherwise returns false.
func (a *ApiConnectionDetails) ArmadaHealthCheck() (ok bool, err error) {
	url := a.ArmadaRestUrl
	if url == "" {
		return false, errors.New("Armada server rest api url not provided")
	}
	if !strings.HasPrefix(url, "http") {
		url = fmt.Sprintf("http://%s", url)
	}
	healthEndpoint := fmt.Sprintf("%s/health", url)
	resp, err := http.Get(healthEndpoint)
	if err != nil {
		return false, errors.WithStack(err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 399 {
		return false, nil
	}

	return true, nil
}
