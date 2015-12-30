package scraper

import (
	"github.com/google/cadvisor/client"
	info "github.com/google/cadvisor/info/v1"
)

type cadvisorInfoProvider struct {
	cc *client.Client
}

func (cip *cadvisorInfoProvider) SubcontainersInfo(containerName string, query *info.ContainerInfoRequest) ([]*info.ContainerInfo, error) {
	infos, err := cip.cc.SubcontainersInfo(containerName, &info.ContainerInfoRequest{NumStats: 3})
	if err != nil {
		return nil, err
	}
	ret := make([]*info.ContainerInfo, len(infos))
	for i, info := range infos {
		containerInfo := info
		ret[i] = &containerInfo
	}
	return ret, nil
}

func (cip *cadvisorInfoProvider) GetVersionInfo() (*info.VersionInfo, error) {
	//TODO: remove fake info
	return &info.VersionInfo{
		KernelVersion:      "4.1.6-200.fc22.x86_64",
		ContainerOsVersion: "Fedora 22 (Twenty Two)",
		DockerVersion:      "1.8.1",
		CadvisorVersion:    "0.16.0",
		CadvisorRevision:   "abcdef",
	}, nil
}

func (cip *cadvisorInfoProvider) GetMachineInfo() (*info.MachineInfo, error) {
	return cip.cc.MachineInfo()
}
