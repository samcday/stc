/*
Copyright 2023 Sam Day.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/go-logr/logr"
	"github.com/golang/protobuf/ptypes/wrappers"
	"github.com/pkg/errors"
	stconfig "github.com/syncthing/syncthing/lib/config"
	"google.golang.org/grpc"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	stcsamcdaycomv1alpha1 "code.samcday.com/me/stc/api/v1alpha1"
	"code.samcday.com/me/stc/controllers"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	//+kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(stcsamcdaycomv1alpha1.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
}

type Driver struct {
	http.RoundTripper
	Client   *http.Client
	Name     string
	Endpoint string
	NodeName string
	Log      logr.Logger
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

func (d Driver) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	var cfg stconfig.Configuration
	resp, err := d.Client.Get(fmt.Sprintf("http://%s/rest/config", d.Endpoint))
	if err != nil {
		return nil, errors.Wrap(err, "failed to GET /rest/config")
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET /rest/config HTTP error %s", resp.Status)
	}
	err = json.NewDecoder(resp.Body).Decode(&cfg)
	if err != nil {
		return nil, errors.Wrap(err, "failed to decode JSON")
	}

	folder := configureFolder(&cfg, req.VolumeId)

	body, err := json.Marshal(folder)
	if err != nil {
		return nil, errors.Wrap(err, "failed to encode JSON")
	}
	resp, err = d.Client.Post(fmt.Sprintf("http://%s/rest/config/folders", d.Endpoint), "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, errors.Wrapf(err, "failed to update Syncthing folder config for %s", req.VolumeId)
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("POST /rest/config/folders/%s HTTP error %s\n%s", req.VolumeId, resp.Status, string(b))
	}

	err = os.MkdirAll(req.TargetPath, 0755)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to mkdir %s", req.TargetPath)
	}

	args := []string{"mount", "--bind", "/var/syncthing/" + req.VolumeId, req.TargetPath}
	cmd := exec.Command("/usr/bin/env", args...)
	err = cmd.Run()
	if err != nil {
		output, _ := cmd.CombinedOutput()
		return nil, errors.Wrapf(err, "%s failed: %s", args, output)
	}
	// Wait until all known devices are online
	// Wait until all devices have been seen more recently than publish start time.
	// Wait until folder last sync time is > publish start time.
	// Done.

	return &csi.NodePublishVolumeResponse{}, nil
}

func configureFolder(cfg *stconfig.Configuration, name string) *stconfig.FolderConfiguration {
	folder, _, _ := cfg.Folder(name)

	// Folder should be offered to all other devices.
	for _, dev := range cfg.Devices {
		folder.Devices = append(folder.Devices, stconfig.FolderDeviceConfiguration{DeviceID: dev.DeviceID})
	}

	folder.ID = name
	folder.Path = "~/" + name

	// Folder should send and receive ownership and extra metadata
	folder.SyncOwnership = true
	folder.SendOwnership = true
	folder.SendXattrs = true
	folder.SyncXattrs = true

	// Watch the filesystem for changes, react within 1 second. Do a full folder scan every 10 minutes.
	// These aggressive defaults will likely result in requests to make this configurable before long.
	folder.FSWatcherEnabled = true
	folder.FSWatcherDelayS = 1
	folder.RescanIntervalS = 600

	// With LIFO sync order I can imagine building some simple sync primitives on top of these semantics.
	// Seems it'd be more friendly to an eventually-consistent shared filesystem view, if RWX were to be supported.
	folder.Order = stconfig.PullOrderOldestFirst
	return &folder
}

func (d Driver) NodeUnpublishVolume(ctx context.Context, request *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	// TODO: unmount
	return &csi.NodeUnpublishVolumeResponse{}, nil
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

type SyncthingTransport struct {
	Transport http.RoundTripper
	APIKey    string
}

func (d SyncthingTransport) RoundTrip(req *http.Request) (resp *http.Response, err error) {
	req.Header.Set("X-API-Key", d.APIKey)
	return d.Transport.RoundTrip(req)
}

//+kubebuilder:rbac:namespace="{{$.Release.Namespace}}",groups="coordination.k8s.io",resources=leases,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:namespace="{{$.Release.Namespace}}",groups="",resources=events,verbs=create;patch

func main() {
	var configFile string
	flag.StringVar(&configFile, "config", "",
		"The controller will load its initial configuration from this file. "+
			"Omit this flag to use the default configuration values. "+
			"Command-line flags override configuration from this file.")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	log := zap.New(zap.UseFlagOptions(&opts))
	ctrl.SetLogger(log)

	csiSocket := os.Getenv("CSI_SOCKET")
	if csiSocket != "" {
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
		server := grpc.NewServer(grpc.UnaryInterceptor(func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp interface{}, err error) {
			if info.FullMethod != "/csi.v1.Identity/Probe" {
				log.V(0).Info("GRPC request", "method", info.FullMethod)
			}
			return handler(ctx, req)
		}))
		driver := &Driver{
			Log:          log,
			Endpoint:     endpoint,
			RoundTripper: http.DefaultTransport,
			Client:       &http.Client{Transport: &SyncthingTransport{APIKey: apiKey, Transport: http.DefaultTransport}},
			Name:         driverName,
			NodeName:     nodeName,
		}
		csi.RegisterIdentityServer(server, driver)
		csi.RegisterNodeServer(server, driver)

		err := os.Remove(csiSocket)
		if err != nil && !os.IsNotExist(err) {
			panic(err)
		}
		l, err := net.Listen("unix", csiSocket)
		if err != nil {
			panic(err)
		}
		fmt.Println("CSI driver listening on " + csiSocket)
		err = server.Serve(l)
		if err != nil {
			panic(err)
		}

		return
	}

	var err error
	options := ctrl.Options{Scheme: scheme}
	if configFile != "" {
		options, err = options.AndFrom(ctrl.ConfigFile().AtPath(configFile))
		if err != nil {
			setupLog.Error(err, "unable to load the config file")
			os.Exit(1)
		}
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), options)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err = (&controllers.SyncthingClusterReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "SyncthingCluster")
		os.Exit(1)
	}
	if err = (&controllers.PVCReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "PVCReconciler")
		os.Exit(1)
	}
	//+kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
