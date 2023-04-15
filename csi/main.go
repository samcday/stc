package main

import (
	"context"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/golang/glog"
	"github.com/golang/protobuf/ptypes/wrappers"
	"google.golang.org/grpc"
	"net"
	"net/http"
	"os"
)

type Driver struct {
	http.RoundTripper
	Name     string
	Endpoint string
	NodeName string
	APIKey   string
}

func (d Driver) GetPluginInfo(ctx context.Context, r *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	return &csi.GetPluginInfoResponse{
		Name:          d.Name,
		VendorVersion: "0", // TODO
	}, nil
}

func (d Driver) GetPluginCapabilities(ctx context.Context, r *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	return &csi.GetPluginCapabilitiesResponse{}, nil
}

func (d Driver) NodeStageVolume(context.Context, *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	panic("unimplemented")
}

func (d Driver) NodeUnstageVolume(context.Context, *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	panic("unimplemented")
}

func (d Driver) NodePublishVolume(ctx context.Context, request *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	panic("implement me")
}

func (d Driver) NodeUnpublishVolume(ctx context.Context, request *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	panic("implement me")
}

func (d Driver) NodeGetVolumeStats(context.Context, *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	panic("unimplemented")
}

func (d Driver) NodeExpandVolume(context.Context, *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	panic("unimplemented")
}

func (d Driver) NodeGetCapabilities(context.Context, *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{}, nil
}

func (d Driver) NodeGetInfo(context.Context, *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{NodeId: d.NodeName}, nil
}

func (d Driver) Probe(context.Context, *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	return &csi.ProbeResponse{Ready: &wrappers.BoolValue{Value: true}}, nil
}

func (d Driver) RoundTrip(req *http.Request) (resp *http.Response, err error) {
	req.Header.Set("X-API-Key", d.APIKey)
	return d.RoundTripper.RoundTrip(req)
}

func main() {
	driverName := os.Getenv("DRIVER_NAME")
	if driverName == "" {
		panic("DRIVER_NAME not set")
	}
	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		panic("NODE_NAME not set")
	}
	endpoint := os.Getenv("ENDPOINT")
	if endpoint == "" {
		panic("ENDPOINT not set")
	}
	apiKey := os.Getenv("STGUIAPIKEY")
	if apiKey == "" {
		panic("STGUIAPIKEY not set")
	}
	socket := os.Getenv("SOCKET")
	if socket == "" {
		socket = "/run/csi/socket"
	}
	server := grpc.NewServer(grpc.UnaryInterceptor(log))
	driver := &Driver{
		Endpoint:     endpoint,
		RoundTripper: http.DefaultTransport,
		Name:         driverName,
		NodeName:     nodeName,
		APIKey:       apiKey,
	}
	csi.RegisterIdentityServer(server, driver)
	csi.RegisterNodeServer(server, driver)

	l, err := net.Listen("unix", socket)
	if err != nil {
		panic(err)
	}
	server.Serve(l)
}

func log(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp interface{}, err error) {
	pri := glog.Level(3)
	if info.FullMethod == "/csi.v1.Identity/Probe" {
		// This call occurs frequently, therefore it only gets log at level 5.
		pri = 5
	}
	glog.V(pri).Infof("GRPC: %s", info.FullMethod)
	return handler(ctx, req)
}
