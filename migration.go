package main

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"os"

	"context"
	"errors"
	"net"
	"path/filepath"

	"github.com/spf13/pflag"
	kubevirtv1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/log"
	"kubevirt.io/kubevirt/pkg/network/namescheme"
	domainSchema "kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/api"

	"google.golang.org/grpc"
	"k8s.io/apimachinery/pkg/util/rand"

	"kubevirt.io/kubevirt/pkg/hooks"
	hooksInfo "kubevirt.io/kubevirt/pkg/hooks/info"
	hooksV1alpha2 "kubevirt.io/kubevirt/pkg/hooks/v1alpha2"
)

const (
	onDefineDomainLoggingMessage  = "OnDefineDomain method has been called"
	preCloudInitIsoLoggingMessage = "PreCloudInitIso method has been called"
	qemuv1NS                      = "http://libvirt.org/schemas/domain/qemu/1.0"
)

type infoServer struct {
	Version string
}

func (s infoServer) Info(ctx context.Context, params *hooksInfo.InfoParams) (*hooksInfo.InfoResult, error) {
	log.Log.Info("Info method has been called")

	var hookPoints = []*hooksInfo.HookPoint{
		{
			Name:     hooksInfo.OnDefineDomainHookPointName,
			Priority: 0,
		},
	}

	return &hooksInfo.InfoResult{
		Name: "shim",
		Versions: []string{
			s.Version,
		},
		HookPoints: hookPoints,
	}, nil
}

type v1Alpha2Server struct{}

func (s v1Alpha2Server) OnDefineDomain(ctx context.Context, params *hooksV1alpha2.OnDefineDomainParams) (*hooksV1alpha2.OnDefineDomainResult, error) {
	log.Log.Info(onDefineDomainLoggingMessage)
	newDomainXML, err := onDefineDomain(params.GetVmi(), params.GetDomainXML())
	if err != nil {
		log.Log.Reason(err).Error("Failed OnDefineDomain")
		return nil, err
	}
	return &hooksV1alpha2.OnDefineDomainResult{
		DomainXML: newDomainXML,
	}, nil
}

func (s v1Alpha2Server) PreCloudInitIso(_ context.Context, _ *hooksV1alpha2.PreCloudInitIsoParams) (*hooksV1alpha2.PreCloudInitIsoResult, error) {
	log.Log.Info(preCloudInitIsoLoggingMessage)
	log.Log.Info("PreCloudInit ISO is a no-op")
	return &hooksV1alpha2.PreCloudInitIsoResult{}, nil
}

func parseCommandLineArgs() (string, error) {
	supportedVersions := []string{"v1alpha1", "v1alpha2"}
	version := ""

	pflag.StringVar(&version, "version", "", "hook version to use")
	pflag.Parse()
	if version == "" {
		return "", fmt.Errorf("Missing --version parameter. Supported options are %s.", supportedVersions)
	}

	supported := false
	for _, v := range supportedVersions {
		if v == version {
			supported = true
			break
		}
	}
	if !supported {
		return "", fmt.Errorf("Version %s is not supported. Supported options are %s.", version, supportedVersions)
	}

	return version, nil
}

func getSocketPath() (string, error) {
	if _, err := os.Stat(hooks.HookSocketsSharedDirectory); err != nil {
		return "", fmt.Errorf("Failed dir %s due %s", hooks.HookSocketsSharedDirectory, err.Error())
	}

	// In case there are multiple shims being used, append random string and try a few times
	for i := 0; i < 10; i++ {
		socketName := fmt.Sprintf("shim-%s.sock", rand.String(4))
		socketPath := filepath.Join(hooks.HookSocketsSharedDirectory, socketName)
		if _, err := os.Stat(socketPath); !errors.Is(err, os.ErrNotExist) {
			log.Log.Infof("Failed socket %s due %s", socketName, err.Error())
			continue
		}
		return socketPath, nil
	}

	return "", fmt.Errorf("Failed generate socket path")
}

func main() {
	log.InitializeLogging("shim-sidecar")

	// Shim arguments
	version, err := parseCommandLineArgs()
	if err != nil {
		log.Log.Reason(err).Errorf("Input error")
		os.Exit(1)
	}

	socketPath, err := getSocketPath()
	if err != nil {
		log.Log.Reason(err).Errorf("Enviroment error")
		os.Exit(1)
	}

	socket, err := net.Listen("unix", socketPath)
	if err != nil {
		log.Log.Reason(err).Errorf("Failed to initialized socket on path: %s", socket)
		os.Exit(1)
	}
	defer os.Remove(socketPath)

	server := grpc.NewServer([]grpc.ServerOption{}...)
	hooksInfo.RegisterInfoServer(server, infoServer{Version: version})
	hooksV1alpha2.RegisterCallbacksServer(server, v1Alpha2Server{})
	log.Log.Infof("shim is now exposing its services on socket %s", socketPath)
	server.Serve(socket)
}

func onDefineDomain(vmiJSON []byte, domainXML []byte) ([]byte, error) {
	log.Log.Info(onDefineDomainLoggingMessage)

	log.Log.Infof("domain xml: %v", string(domainXML))
	domainSpec := domainSchema.DomainSpec{}
	err := xml.Unmarshal(domainXML, &domainSpec)
	if err != nil {
		log.Log.Reason(err).Errorf("Failed to unmarshal given domain spec: %s", domainXML)
		panic(err)
	}

	vmi := &kubevirtv1.VirtualMachineInstance{}
	err = json.Unmarshal(vmiJSON, vmi)
	if err != nil {
		log.Log.Reason(err).Errorf("Failed to unmarshal vmi json")
	}

	networkMap := namescheme.CreateHashedNetworkNameScheme(vmi.Spec.Networks)
	macMap := generateNetworkMacMap(vmi)
	for i, v := range domainSpec.Devices.Interfaces {
		if v.MAC != nil && v.Target != nil {
			log.Log.Infof("looking up interface %s", v.Target.Device)
			networkName, ok := macMap[v.MAC.MAC]
			if ok {
				hashedName, innerOK := networkMap[networkName]
				if innerOK {
					newTAPIfName := "tap" + hashedName[3:]
					log.Log.Infof("updating tap interfaceName %s to %s", domainSpec.Devices.Interfaces[i].Target.Device, newTAPIfName)
					domainSpec.Devices.Interfaces[i].Target.Device = "tap" + hashedName[3:]
				}
			}
		}

	}
	newDomainXML, err := xml.Marshal(domainSpec)
	if err != nil {
		log.Log.Reason(err).Errorf("Failed to marshal updated domain spec: %+v", domainSpec)
		panic(err)
	}

	log.Log.Info("Successfully updated original domain spec with requested mac attributes")

	return newDomainXML, nil
}

func generateNetworkMacMap(vmi *kubevirtv1.VirtualMachineInstance) map[string]string {
	networkMacMapping := make(map[string]string)
	for _, v := range vmi.Status.Interfaces {
		networkMacMapping[v.MAC] = v.Name
	}
	return networkMacMapping
}
