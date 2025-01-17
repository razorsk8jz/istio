// Copyright 2018 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package client_test

import (
	"fmt"
	"net"
	"testing"

	v2 "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	auth "github.com/envoyproxy/go-control-plane/envoy/api/v2/auth"
	core "github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	listener "github.com/envoyproxy/go-control-plane/envoy/api/v2/listener"
	route "github.com/envoyproxy/go-control-plane/envoy/api/v2/route"
	hcm "github.com/envoyproxy/go-control-plane/envoy/config/filter/network/http_connection_manager/v2"
	discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v2"
	"github.com/envoyproxy/go-control-plane/pkg/cache"
	xds "github.com/envoyproxy/go-control-plane/pkg/server"
	"github.com/envoyproxy/go-control-plane/pkg/wellknown"

	"google.golang.org/grpc"

	meshconfig "istio.io/api/mesh/v1alpha1"

	"istio.io/istio/mixer/test/client/env"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/networking/plugin"
	"istio.io/istio/pilot/pkg/networking/plugin/mixer"
	pilotutil "istio.io/istio/pilot/pkg/networking/util"
	"istio.io/istio/pkg/config/host"
	"istio.io/istio/pkg/config/labels"
	"istio.io/istio/pkg/proto"
)

const (
	envoyConf = `
admin:
  access_log_path: {{.AccessLogPath}}
  address:
    socket_address:
      address: 127.0.0.1
      port_value: {{.Ports.AdminPort}}
node:
  id: id
  cluster: unknown
dynamic_resources:
  lds_config: { ads: {} }
  ads_config:
    api_type: GRPC
    grpc_services:
      envoy_grpc:
        cluster_name: xds
static_resources:
  clusters:
  - name: xds
    http2_protocol_options: {}
    connect_timeout: 5s
    type: STATIC
    hosts:
    - socket_address:
        address: 127.0.0.1
        port_value: {{.Ports.DiscoveryPort}}
  - name: "outbound|||svc.ns3"
    connect_timeout: 5s
    type: STATIC
    hosts:
    - socket_address:
        address: 127.0.0.1
        port_value: {{.Ports.ServerProxyPort}}
    tls_context:
      common_tls_context:
        tls_certificates:
          certificate_chain:
            filename: testdata/client.cert
          private_key:
            filename: testdata/client-key.cert
        validation_context:
          trusted_ca:
            filename: testdata/root.cert
          verify_subject_alt_name: ["spiffe://cluster.local/ns/default/sa/server"]
      sni: istio.io
  - name: "inbound|||backend"
    connect_timeout: 5s
    type: STATIC
    hosts:
    - socket_address:
        address: 127.0.0.1
        port_value: {{.Ports.BackendPort}}
  - name: "outbound|9091||mixer_server"
    http2_protocol_options: {}
    connect_timeout: 5s
    type: STATIC
    hosts:
    - socket_address:
        address: 127.0.0.1
        port_value: {{.Ports.MixerPort}}
`

	checkAttributesOkOutbound = `
{
  "connection.mtls": false,
  "origin.ip": "[127 0 0 1]",
  "context.protocol": "http",
  "context.reporter.kind": "outbound",
  "context.reporter.uid": "kubernetes://pod2.ns2",
  "destination.service.host": "svc.ns3",
  "destination.service.name": "svc",
  "destination.service.namespace": "ns3",
  "destination.service.uid": "istio://ns3/services/svc",
  "source.uid": "kubernetes://pod2.ns2",
  "source.namespace": "ns2",
  "request.headers": {
     ":method": "GET",
     ":path": "/echo",
     ":authority": "*",
     "x-forwarded-proto": "http",
     "x-request-id": "*"
  },
  "request.host": "*",
  "request.path": "/echo",
  "request.time": "*",
  "request.useragent": "Go-http-client/1.1",
  "request.method": "GET",
  "request.scheme": "http",
  "request.url_path": "/echo"
}
`

	checkAttributesOkInbound = `
{
  "connection.mtls": true,
  "connection.requested_server_name": "istio.io",
  "destination.principal": "cluster.local/ns/default/sa/server",
  "origin.ip": "[127 0 0 1]",
  "context.protocol": "http",
  "context.reporter.kind": "inbound",
  "context.reporter.uid": "kubernetes://pod1.ns2",
  "destination.ip": "[0 0 0 0 0 0 0 0 0 0 255 255 127 0 0 1]",
  "destination.port": "*",
  "destination.namespace": "ns2",
  "destination.uid": "kubernetes://pod1.ns2",
  "destination.service.host": "svc.ns3",
  "destination.service.name": "svc",
  "destination.service.namespace": "ns3",
  "destination.service.uid": "istio://ns3/services/svc",
  "source.principal": "cluster.local/ns/default/sa/client",
  "source.uid": "kubernetes://pod2.ns2",
  "request.headers": {
     ":method": "GET",
     ":path": "/echo",
     ":authority": "*",
     "x-forwarded-proto": "http",
     "x-request-id": "*"
  },
  "request.host": "*",
  "request.path": "/echo",
  "request.time": "*",
  "request.useragent": "Go-http-client/1.1",
  "request.method": "GET",
  "request.scheme": "http",
  "request.url_path": "/echo"
}
`
	reportAttributesOkOutbound = `
{
  "connection.mtls": false,
  "origin.ip": "[127 0 0 1]",
  "context.protocol": "http",
  "context.proxy_error_code": "-",
  "context.reporter.kind": "outbound",
  "context.reporter.uid": "kubernetes://pod2.ns2",
  "destination.ip": "[127 0 0 1]",
  "destination.port": "*",
  "destination.service.host": "svc.ns3",
  "destination.service.name": "svc",
  "destination.service.namespace": "ns3",
  "destination.service.uid": "istio://ns3/services/svc",
  "source.uid": "kubernetes://pod2.ns2",
  "source.namespace": "ns2",
  "check.cache_hit": false,
  "quota.cache_hit": false,
  "request.headers": {
     ":method": "GET",
     ":path": "/echo",
     ":authority": "*",
     "x-forwarded-proto": "http",
     "x-istio-attributes": "-",
     "x-request-id": "*"
  },
  "request.host": "*",
  "request.path": "/echo",
  "request.time": "*",
  "request.useragent": "Go-http-client/1.1",
  "request.method": "GET",
  "request.scheme": "http",
  "request.size": 0,
  "request.total_size": "*",
  "response.time": "*",
  "response.size": 0,
  "response.duration": "*",
  "response.code": 200,
  "response.headers": {
     "date": "*",
     "content-length": "0",
     ":status": "200",
     "server": "envoy"
  },
  "response.total_size": "*",
  "request.url_path": "/echo"
}`

	reportAttributesOkInbound = `
{
  "connection.mtls": true,
  "connection.requested_server_name": "istio.io",
  "destination.principal": "cluster.local/ns/default/sa/server",
  "origin.ip": "[127 0 0 1]",
  "context.protocol": "http",
  "context.proxy_error_code": "-",
  "context.reporter.kind": "inbound",
  "context.reporter.uid": "kubernetes://pod1.ns2",
  "destination.ip": "[0 0 0 0 0 0 0 0 0 0 255 255 127 0 0 1]",
  "destination.port": "*",
  "destination.namespace": "ns2",
  "destination.uid": "kubernetes://pod1.ns2",
  "destination.service.host": "svc.ns3",
  "destination.service.name": "svc",
  "destination.service.namespace": "ns3",
  "destination.service.uid": "istio://ns3/services/svc",
  "source.uid": "kubernetes://pod2.ns2",
  "check.cache_hit": false,
  "quota.cache_hit": false,
  "request.headers": {
     ":method": "GET",
     ":path": "/echo",
     ":authority": "*",
     "x-forwarded-proto": "http",
     "x-istio-attributes": "-",
     "x-request-id": "*"
  },
  "request.host": "*",
  "request.path": "/echo",
  "request.time": "*",
  "request.useragent": "Go-http-client/1.1",
  "request.method": "GET",
  "request.scheme": "http",
  "request.size": 0,
  "request.total_size": "*",
  "response.time": "*",
  "response.size": 0,
  "response.duration": "*",
  "response.code": 200,
  "response.headers": {
     "date": "*",
     "content-length": "0",
     ":status": "200",
     "server": "envoy"
  },
  "response.total_size": "*",
  "request.url_path": "/echo",
  "source.principal": "cluster.local/ns/default/sa/client"
}`
)

func TestPilotPlugin(t *testing.T) {
	s := env.NewTestSetup(env.PilotPluginTLSTest, t)
	s.EnvoyTemplate = envoyConf
	grpcServer := grpc.NewServer()
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", s.Ports().DiscoveryPort))
	if err != nil {
		t.Fatal(err)
	}

	snapshots := cache.NewSnapshotCache(true, mock{}, nil)
	_ = snapshots.SetSnapshot(id, makeSnapshot(s, t))
	server := xds.NewServer(snapshots, nil)
	discovery.RegisterAggregatedDiscoveryServiceServer(grpcServer, server)
	go func() {
		_ = grpcServer.Serve(lis)
	}()
	defer grpcServer.GracefulStop()

	if err := s.SetUp(); err != nil {
		t.Fatalf("Failed to setup test: %v", err)
	}
	defer s.TearDown()

	s.WaitEnvoyReady()

	// Issues a GET echo request with 0 size body
	if _, _, err := env.HTTPGet(fmt.Sprintf("http://localhost:%d/echo", s.Ports().ClientProxyPort)); err != nil {
		t.Errorf("Failed in request: %v", err)
	}
	s.VerifyCheck("http-outbound", checkAttributesOkOutbound)
	s.VerifyCheck("http-inbound", checkAttributesOkInbound)
	s.VerifyTwoReports("http", reportAttributesOkOutbound, reportAttributesOkInbound)
}

type mock struct{}

func (mock) ID(*core.Node) string {
	return id
}
func (mock) GetProxyServiceInstances(_ *model.Proxy) ([]*model.ServiceInstance, error) {
	return nil, nil
}
func (mock) GetProxyWorkloadLabels(proxy *model.Proxy) (labels.Collection, error) {
	return nil, nil
}
func (mock) GetService(_ host.Name) (*model.Service, error) { return nil, nil }
func (mock) InstancesByPort(_ *model.Service, _ int, _ labels.Collection) ([]*model.ServiceInstance, error) {
	return nil, nil
}
func (mock) ManagementPorts(_ string) model.PortList                        { return nil }
func (mock) Services() ([]*model.Service, error)                            { return nil, nil }
func (mock) WorkloadHealthCheckInfo(_ string) model.ProbeList               { return nil }
func (mock) GetIstioServiceAccounts(_ *model.Service, ports []int) []string { return nil }

const (
	id = "id"
)

var (
	svc = model.Service{
		Hostname: "svc.ns3",
		Attributes: model.ServiceAttributes{
			Name:      "svc",
			Namespace: "ns3",
			UID:       "istio://ns3/services/svc",
		},
	}
	pushContext = model.PushContext{
		ServiceByHostnameAndNamespace: map[host.Name]map[string]*model.Service{
			host.Name("svc.ns3"): {
				"ns3": &svc,
			},
		},
		Mesh: &meshconfig.MeshConfig{
			MixerCheckServer:            "mixer_server:9091",
			MixerReportServer:           "mixer_server:9091",
			EnableClientSidePolicyCheck: true,
		},
		ServiceDiscovery: mock{},
	}
	serverParams = plugin.InputParams{
		ListenerProtocol: plugin.ListenerProtocolHTTP,
		Node: &model.Proxy{
			ID:       "pod1.ns2",
			Type:     model.SidecarProxy,
			Metadata: &model.NodeMetadata{},
		},
		ServiceInstance: &model.ServiceInstance{Service: &svc},
		Push:            &pushContext,
	}
	clientParams = plugin.InputParams{
		ListenerProtocol: plugin.ListenerProtocolHTTP,
		Node: &model.Proxy{
			ID:       "pod2.ns2",
			Type:     model.SidecarProxy,
			Metadata: &model.NodeMetadata{},
		},
		Service: &svc,
		Push:    &pushContext,
	}
)

func makeRoute(cluster string) *v2.RouteConfiguration {
	return &v2.RouteConfiguration{
		Name: cluster,
		VirtualHosts: []*route.VirtualHost{{
			Name:    cluster,
			Domains: []string{"*"},
			Routes: []*route.Route{{
				Match: &route.RouteMatch{PathSpecifier: &route.RouteMatch_Prefix{Prefix: "/"}},
				Action: &route.Route_Route{Route: &route.RouteAction{
					ClusterSpecifier: &route.RouteAction_Cluster{Cluster: cluster},
				}},
			}},
		}},
	}
}

func makeListener(port uint16, route string) (*v2.Listener, *hcm.HttpConnectionManager) {
	return &v2.Listener{
			Name: route,
			Address: &core.Address{Address: &core.Address_SocketAddress{SocketAddress: &core.SocketAddress{
				Address:       "127.0.0.1",
				PortSpecifier: &core.SocketAddress_PortValue{PortValue: uint32(port)}}}},
			ListenerFilters: []*listener.ListenerFilter{{
				Name: "envoy.listener.tls_inspector",
			}},
		}, &hcm.HttpConnectionManager{
			CodecType:  hcm.HttpConnectionManager_AUTO,
			StatPrefix: route,
			RouteSpecifier: &hcm.HttpConnectionManager_Rds{
				Rds: &hcm.Rds{RouteConfigName: route, ConfigSource: &core.ConfigSource{
					ConfigSourceSpecifier: &core.ConfigSource_Ads{Ads: &core.AggregatedConfigSource{}},
				}},
			},
			HttpFilters: []*hcm.HttpFilter{{Name: wellknown.Router}},
		}
}

func makeSnapshot(s *env.TestSetup, t *testing.T) cache.Snapshot {
	clientListener, clientManager := makeListener(s.Ports().ClientProxyPort, "outbound|||svc.ns3")
	serverListener, serverManager := makeListener(s.Ports().ServerProxyPort, "inbound|||backend")
	clientRoute := makeRoute("outbound|||svc.ns3")
	serverRoute := makeRoute("inbound|||backend")

	p := mixer.NewPlugin()

	serverMutable := plugin.MutableObjects{Listener: serverListener, FilterChains: []plugin.FilterChain{{}}}
	if err := p.OnInboundListener(&serverParams, &serverMutable); err != nil {
		t.Error(err)
	}
	serverManager.HttpFilters = append(serverMutable.FilterChains[0].HTTP, serverManager.HttpFilters...)
	serverListener.FilterChains = []*listener.FilterChain{{
		Filters: []*listener.Filter{{
			Name:       wellknown.HTTPConnectionManager,
			ConfigType: &listener.Filter_TypedConfig{TypedConfig: pilotutil.MessageToAny(serverManager)},
		}},
		// turn on mTLS on downstream
		TlsContext: &auth.DownstreamTlsContext{
			CommonTlsContext: &auth.CommonTlsContext{
				TlsCertificates: []*auth.TlsCertificate{{
					CertificateChain: &core.DataSource{Specifier: &core.DataSource_Filename{Filename: "testdata/server.cert"}},
					PrivateKey:       &core.DataSource{Specifier: &core.DataSource_Filename{Filename: "testdata/server-key.cert"}},
				}},
				ValidationContextType: &auth.CommonTlsContext_ValidationContext{
					ValidationContext: &auth.CertificateValidationContext{
						TrustedCa: &core.DataSource{Specifier: &core.DataSource_Filename{Filename: "testdata/root.cert"}},
					},
				},
			},
			RequireClientCertificate: proto.BoolTrue,
		},
	}}

	clientMutable := plugin.MutableObjects{Listener: clientListener, FilterChains: []plugin.FilterChain{{}}}
	if err := p.OnOutboundListener(&clientParams, &clientMutable); err != nil {
		t.Error(err)
	}
	clientManager.HttpFilters = append(clientMutable.FilterChains[0].HTTP, clientManager.HttpFilters...)
	clientListener.FilterChains = []*listener.FilterChain{{Filters: []*listener.Filter{{
		Name:       wellknown.HTTPConnectionManager,
		ConfigType: &listener.Filter_TypedConfig{TypedConfig: pilotutil.MessageToAny(clientManager)},
	}}}}

	p.OnInboundRouteConfiguration(&serverParams, serverRoute)
	p.OnOutboundRouteConfiguration(&clientParams, clientRoute)

	return cache.Snapshot{
		Routes:    cache.NewResources("http", []cache.Resource{clientRoute, serverRoute}),
		Listeners: cache.NewResources("http", []cache.Resource{clientListener, serverListener}),
	}
}
