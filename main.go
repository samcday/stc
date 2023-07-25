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
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/go-logr/logr"
	"github.com/golang/protobuf/ptypes/wrappers"
	"github.com/pkg/errors"
	stconfig "github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/protocol"
	"google.golang.org/grpc"
	"k8s.io/apimachinery/pkg/util/wait"
	klog "sigs.k8s.io/controller-runtime/pkg/log"
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
	Client        *http.Client
	Name          string
	Endpoint      string
	NodeName      string
	Log           logr.Logger
	Mu            sync.Mutex
	LastPublished map[string]time.Time
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

func runCmd(ctx context.Context, log logr.Logger, cmd string) error {
	log.V(1).Info("running command", "cmd", cmd)
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	return c.Run()
}
func (d Driver) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	d.Mu.Lock()
	start := time.Now()
	defer d.Mu.Unlock()
	log := klog.FromContext(ctx, "volume-id", req.VolumeId)
	log.V(2).Info("publishing volume")

	//lp, ok := d.LastPublished[req.VolumeId]
	//if ok && time.Since(lp) < 5*time.Second {
	//	log.V(1).Info("skipping NodePublishVolume")
	//	return &csi.NodePublishVolumeResponse{}, nil
	//}

	var cfg stconfig.Configuration
	cfgUrl := fmt.Sprintf("http://%s/rest/config", d.Endpoint)
	resp, err := d.Client.Get(cfgUrl)
	if err != nil {
		return nil, errors.Wrap(err, "failed to GET "+cfgUrl)
	}
	if resp.StatusCode != 200 && resp.StatusCode != 404 {
		return nil, fmt.Errorf("GET %s HTTP error %s", cfgUrl, resp.Status)
	} else if resp.StatusCode != 404 {
		err = json.NewDecoder(resp.Body).Decode(&cfg)
		if err != nil {
			return nil, errors.Wrap(err, "failed to decode JSON")
		}
	}

	err = d.configureFolder(ctx, log, &cfg, req.VolumeId)
	if err != nil {
		return nil, err
	}

	stat, err := os.Stat(req.TargetPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, errors.Wrapf(err, "failed to stat '%s'", req.TargetPath)
	}
	if stat == nil {
		log.Info("making target dir", "path", req.TargetPath)
		err = os.MkdirAll(req.TargetPath, 0755)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to mkdir %s", req.TargetPath)
		}
	} else if !stat.IsDir() {
		return nil, errors.Errorf("path '%s' already exists and is not a directory", req.TargetPath)
	}

	var status struct {
		MyID string `json:"myID"`
	}
	statusUrl := fmt.Sprintf("http://%s/rest/system/status", d.Endpoint)
	resp, err = d.Client.Get(statusUrl)
	if err == nil && resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP error %s", resp.Status)
	}
	if err != nil {
		return nil, errors.Wrap(err, "failed to GET "+statusUrl)
	}
	err = json.NewDecoder(resp.Body).Decode(&status)
	if err != nil {
		return nil, errors.Wrap(err, "failed to decode JSON")
	}

	err = wait.Poll(time.Second, time.Minute*2, func() (done bool, err error) {
		var deviceStatus map[string]struct {
			LastSeen time.Time `json:"lastSeen"`
		}
		url := fmt.Sprintf("http://%s/rest/stats/device", d.Endpoint)
		resp, err = d.Client.Get(url)
		if err == nil && resp.StatusCode != 200 {
			err = fmt.Errorf("HTTP error %s", resp.Status)
		}
		if err != nil {
			err = errors.Wrap(err, "failed to GET "+url)
			return
		}
		err = json.NewDecoder(resp.Body).Decode(&deviceStatus)
		if err != nil {
			err = errors.Wrap(err, "failed to decode JSON")
			return
		}

		done = true
		for id, v := range deviceStatus {
			if id == status.MyID {
				continue
			}
			if v.LastSeen.Before(start) {
				done = false
				break
			}
		}
		return
	})
	if err != nil {
		return nil, errors.Wrap(err, "failed to wait for device readiness")
	}

	// Wait until folder reports 100% completion
	err = wait.Poll(time.Second, time.Minute*2, func() (done bool, err error) {
		var completion struct {
			Completion float64 `json:"completion"`
		}
		url := fmt.Sprintf("http://%s/rest/db/completion?folder=%s", d.Endpoint, req.VolumeId)
		resp, err = d.Client.Get(url)
		if err == nil && resp.StatusCode != 200 {
			err = fmt.Errorf("HTTP error %s", resp.Status)
		}
		if err != nil {
			err = errors.Wrap(err, "failed to GET "+url)
			return
		}
		err = json.NewDecoder(resp.Body).Decode(&completion)
		if err != nil {
			err = errors.Wrap(err, "failed to decode JSON")
			return
		}
		done = completion.Completion == 100
		return
	})
	if err != nil {
		return nil, errors.Wrap(err, "failed to wait for folder readiness")
	}

	unMounted := false
	err = runCmd(ctx, log, fmt.Sprintf("findmnt %s", req.TargetPath))
	if exitError, ok := err.(*exec.ExitError); ok {
		unMounted = exitError.ExitCode() == 1
	} else if err != nil {
		return nil, errors.Wrap(err, "findmnt failed")
	}
	if unMounted {
		log.Info("mounting volume")
		err = runCmd(ctx, log, fmt.Sprintf("mount --bind /var/syncthing/%s %s", req.VolumeId, req.TargetPath))
		if err != nil {
			return nil, errors.Wrap(err, "failed to mount")
		}
	}

	d.LastPublished[req.VolumeId] = time.Now()

	return &csi.NodePublishVolumeResponse{}, nil
}

func (d Driver) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	d.Mu.Lock()
	defer d.Mu.Unlock()
	log := klog.FromContext(ctx, "req", req)
	log.Info("unpublishing volume")

	mounted := true
	err := runCmd(ctx, log, fmt.Sprintf("findmnt %s", req.TargetPath))
	if err != nil {
		mounted = false
	}

	if mounted {
		err := runCmd(ctx, log, fmt.Sprintf("umount %s", req.TargetPath))
		if err != nil {
			return nil, errors.Wrapf(err, "failed to stat '%s'", req.TargetPath)
		}
	}

	stat, err := os.Stat(req.TargetPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, errors.Wrapf(err, "failed to stat '%s'", req.TargetPath)
	}
	if stat != nil {
		log.Info("deleting target dir", "path", req.TargetPath)
		err = os.Remove(req.TargetPath)
		if err != nil && !os.IsNotExist(err) {
			return nil, errors.Wrapf(err, "failed to rmdir %s", req.TargetPath)
		}
	}

	delete(d.LastPublished, req.VolumeId)
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

func (d Driver) configureFolder(ctx context.Context, log logr.Logger, cfg *stconfig.Configuration, name string) error {
	folder, _, _ := cfg.Folder(name)
	clone := folder.Copy()
	// Folder should be offered to all other devices.
	for _, device := range cfg.Devices {
		found := false
		for _, v := range folder.Devices {
			if v.DeviceID == device.DeviceID {
				found = true
				break
			}
		}
		if !found {
			folder.Devices = append(folder.Devices, stconfig.FolderDeviceConfiguration{DeviceID: device.DeviceID})
		}
	}

	folder.ID = name
	folder.Path = "~/" + name

	// Folder should send and receive ownership and extra metadata
	folder.SyncOwnership = true
	folder.SendOwnership = true
	folder.SendXattrs = true
	folder.SyncXattrs = true

	folder.FSWatcherEnabled = true
	folder.FSWatcherDelayS = 10
	folder.RescanIntervalS = 3600

	// With LIFO sync order I can imagine building some simple sync primitives on top of these semantics.
	// Seems it'd be more friendly to an eventually-consistent shared filesystem view, if RWX were to be supported.
	folder.Order = stconfig.PullOrderOldestFirst

	if clone.String() != folder.String() {
		log.Info("updating folder configuration")
		body, err := json.Marshal(folder)
		if err != nil {
			return errors.Wrap(err, "failed to encode JSON")
		}
		folderUrl := fmt.Sprintf("http://%s/rest/config/folders/%s", d.Endpoint, name)
		r, err := http.NewRequestWithContext(ctx, "PUT", folderUrl, bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if err != nil {
			return errors.Wrap(err, "failed to create request")
		}
		resp, err := d.Client.Do(r)
		if err != nil {
			return errors.Wrapf(err, "failed to update Syncthing folder config for %s", name)
		}
		if resp.StatusCode != 200 {
			b, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("POST /rest/config/folders/%s HTTP error %s\n%s", name, resp.Status, string(b))
		}
	}
	return nil
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
			Log:           log,
			Endpoint:      endpoint,
			RoundTripper:  http.DefaultTransport,
			Client:        &http.Client{Transport: &SyncthingTransport{APIKey: apiKey, Transport: http.DefaultTransport}},
			Name:          driverName,
			NodeName:      nodeName,
			LastPublished: map[string]time.Time{},
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

func mustDID(raw string) protocol.DeviceID {
	did, err := protocol.DeviceIDFromString(raw)
	if err != nil {
		panic(err)
	}
	return did
}
