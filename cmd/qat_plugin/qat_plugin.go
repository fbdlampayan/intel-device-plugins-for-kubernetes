// Copyright 2017 Intel Corporation. All Rights Reserved.
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

package main

import (
	"bytes"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/golang/glog"

	pluginapi "k8s.io/kubernetes/pkg/kubelet/apis/deviceplugin/v1beta1"

	"github.com/intel/intel-device-plugins-for-kubernetes/internal/deviceplugin"
)

const (
	uioDevicePath         = "/dev"
	vfioDevicePath        = "/dev/vfio"
	uioMountPath          = "/sys/class/uio"
	pciDeviceDirectory    = "/sys/bus/pci/devices"
	pciDriverDirectory    = "/sys/bus/pci/drivers"
	uioSuffix             = "uio"
	iommuGroupSuffix      = "iommu_group"
	sysfsIommuGroupPrefix = "/sys/kernel/iommu_groups"
	newIDSuffix           = "new_id"
	driverUnbindSuffix    = "driver/unbind"
	qatDeviceRE           = "[0-9|a-f][0-9|a-f]:[0-9|a-f][0-9|a-f]\\.[0-9|a-f].*"
	vendorPrefix          = "8086 "

	namespace = "qat"
)

type devicePlugin struct {
	maxDevices      int
	pciDriverDir    string
	pciDeviceDir    string
	kernelVfDrivers []string
	dpdkDriver      string
}

func newDevicePlugin(pciDriverDir, pciDeviceDir string, maxDevices int, kernelVfDrivers []string, dpdkDriver string) *devicePlugin {
	return &devicePlugin{
		maxDevices:      maxDevices,
		pciDriverDir:    pciDriverDir,
		pciDeviceDir:    pciDeviceDir,
		kernelVfDrivers: kernelVfDrivers,
		dpdkDriver:      dpdkDriver,
	}
}

func (dp *devicePlugin) Scan(notifier deviceplugin.Notifier) error {
	for {
		devTree, err := dp.scan()
		if err != nil {
			glog.Error("Device scan failed: ", err)
			return fmt.Errorf("Device scan failed: %v", err)
		}

		notifier.Notify(devTree)

		time.Sleep(5 * time.Second)
	}
}

func (dp *devicePlugin) getDpdkDevice(id string) (string, error) {

	devicePCIAdd := "0000:" + id
	switch dp.dpdkDriver {
	// TODO: case "pci-generic" and "kernel":
	case "igb_uio":
		uioDirPath := path.Join(dp.pciDeviceDir, devicePCIAdd, uioSuffix)
		files, err := ioutil.ReadDir(uioDirPath)
		if err != nil {
			return "", err
		}
		if len(files) == 0 {
			return "", fmt.Errorf("No devices found")
		}
		return files[0].Name(), nil

	case "vfio-pci":
		vfioDirPath := path.Join(dp.pciDeviceDir, devicePCIAdd, iommuGroupSuffix)
		group, err := filepath.EvalSymlinks(vfioDirPath)
		if err != nil {
			return "", err
		}
		s := strings.TrimPrefix(group, sysfsIommuGroupPrefix)
		fmt.Printf("The vfio device group detected is %v\n", s)
		return s, nil
	}

	return "", fmt.Errorf("Unknown DPDK driver")
}

func (dp *devicePlugin) getDpdkDeviceNames(id string) ([]string, error) {
	dpdkDeviceName, err := dp.getDpdkDevice(id)
	if err != nil {
		return []string{}, fmt.Errorf("Unable to get the dpdk device for creating device nodes: %v", err)
	}
	fmt.Printf("%s device: corresponding DPDK device detected is %s\n", id, dpdkDeviceName)

	switch dp.dpdkDriver {
	// TODO: case "pci-generic" and "kernel":
	case "igb_uio":
		//Setting up with uio
		uioDev := path.Join(uioDevicePath, dpdkDeviceName)
		return []string{uioDev}, nil
	case "vfio-pci":
		//Setting up with vfio
		vfioDev1 := path.Join(vfioDevicePath, dpdkDeviceName)
		vfioDev2 := path.Join(vfioDevicePath, "/vfio")
		return []string{vfioDev1, vfioDev2}, nil
	}

	return []string{}, fmt.Errorf("Unknown DPDK driver")
}

func (dp *devicePlugin) getDpdkMountPaths(id string) ([]string, error) {
	dpdkDeviceName, err := dp.getDpdkDevice(id)
	if err != nil {
		return []string{}, fmt.Errorf("Unable to get the dpdk device for mountPath: %v", err)
	}

	switch dp.dpdkDriver {
	case "igb_uio":
		//Setting up with uio mountpoints
		uioMountPoint := path.Join(uioMountPath, dpdkDeviceName, "/device")
		return []string{uioMountPoint}, nil
	case "vfio-pci":
		//No mountpoint for vfio needs to be populated
		return []string{}, nil
	}

	return nil, fmt.Errorf("Unknown DPDK driver")
}

func (dp *devicePlugin) getDeviceID(pciAddr string) (string, error) {
	devID, err := ioutil.ReadFile(path.Join(dp.pciDeviceDir, pciAddr, "device"))
	if err != nil {
		return "", fmt.Errorf("Cannot obtain ID for the device %s: %v", pciAddr, err)
	}

	return strings.TrimPrefix(string(bytes.TrimSpace(devID)), "0x"), nil
}

// bindDevice unbinds given device from kernel driver and binds to DPDK driver
func (dp *devicePlugin) bindDevice(id string) error {
	devicePCIAddr := "0000:" + id
	unbindDevicePath := path.Join(dp.pciDeviceDir, devicePCIAddr, driverUnbindSuffix)

	// Unbind from the kernel driver
	err := ioutil.WriteFile(unbindDevicePath, []byte(devicePCIAddr), 0644)
	if err != nil {
		return fmt.Errorf("Unbinding from kernel driver failed for the device %s: %v", id, err)

	}

	vfdevID, err := dp.getDeviceID(devicePCIAddr)
	if err != nil {
		return fmt.Errorf("Cannot obtain ID for the device %s: %v", id, err)
	}
	bindDevicePath := path.Join(dp.pciDriverDir, dp.dpdkDriver, newIDSuffix)
	//Bind to the the dpdk driver
	err = ioutil.WriteFile(bindDevicePath, []byte(vendorPrefix+vfdevID), 0644)
	if err != nil {
		return fmt.Errorf("Binding to the DPDK driver failed for the device %s: %v", id, err)
	}

	return nil
}

func isValidKerneDriver(kernelvfDriver string) bool {
	switch kernelvfDriver {
	case "dh895xccvf", "c6xxvf", "c3xxxvf", "d15xxvf":
		return true
	}
	return false
}

func isValidDpdkDeviceDriver(dpdkDriver string) bool {
	switch dpdkDriver {
	case "igb_uio", "vfio-pci":
		return true
	}
	return false
}

func (dp *devicePlugin) scan() (deviceplugin.DeviceTree, error) {
	devTree := deviceplugin.NewDeviceTree()

	for _, driver := range append(dp.kernelVfDrivers, dp.dpdkDriver) {
		files, err := ioutil.ReadDir(path.Join(dp.pciDriverDir, driver))
		if err != nil {
			return nil, fmt.Errorf("Can't read sysfs for driver %s: %+v", driver, err)
		}

		n := 0
		for _, file := range files {
			if !strings.HasPrefix(file.Name(), "0000:") {
				continue
			}
			n = n + 1 // increment after all junk got filtered out

			if n > dp.maxDevices {
				break
			}

			vfpciaddr := strings.TrimPrefix(file.Name(), "0000:")

			// initialize newly found devices which aren't bound to DPDK driver yet
			if driver != dp.dpdkDriver {
				err := dp.bindDevice(vfpciaddr)
				if err != nil {
					return nil, fmt.Errorf("Error in binding the device to the dpdk driver: %+v", err)
				}
			}

			devNodes, err := dp.getDpdkDeviceNames(vfpciaddr)
			if err != nil {
				return nil, fmt.Errorf("Error in obtaining the device name: %+v", err)
			}
			devMounts, err := dp.getDpdkMountPaths(vfpciaddr)
			if err != nil {
				return nil, fmt.Errorf("Error in obtaining the mount point: %+v", err)
			}

			devinfo := deviceplugin.DeviceInfo{
				State:  pluginapi.Healthy,
				Nodes:  devNodes,
				Mounts: devMounts,
				Envs: map[string]string{
					fmt.Sprintf("%s%d", namespace, n): file.Name(),
				},
			}

			devTree.AddDevice("generic", vfpciaddr, devinfo)
		}
	}

	return devTree, nil
}

func main() {
	dpdkDriver := flag.String("dpdk-driver", "igb_uio", "DPDK Device driver for configuring the QAT device")
	kernelVfDrivers := flag.String("kernel-vf-drivers", "dh895xccvf,c6xxvf,c3xxxvf,d15xxvf", "Comma separated VF Device Driver of the QuickAssist Devices in the system. Devices supported: DH895xCC,C62x,C3xxx and D15xx")
	maxNumDevices := flag.Int("max-num-devices", 32, "maximum number of QAT devices to be provided to the QuickAssist device plugin")
	flag.Parse()
	fmt.Println("QAT device plugin started")

	if !isValidDpdkDeviceDriver(*dpdkDriver) {
		fmt.Println("Wrong DPDK device driver:", *dpdkDriver)
		os.Exit(1)
	}

	kernelDrivers := strings.Split(*kernelVfDrivers, ",")
	for _, driver := range kernelDrivers {
		if !isValidKerneDriver(driver) {
			fmt.Println("Wrong kernel VF driver:", driver)
			os.Exit(1)
		}
	}

	plugin := newDevicePlugin(pciDriverDirectory, pciDeviceDirectory, *maxNumDevices, kernelDrivers, *dpdkDriver)
	manager := deviceplugin.NewManager(namespace, plugin)
	manager.Run()
}
