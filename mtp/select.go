package mtp

/*
#include "imagecapture_bridge.h"
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"unsafe"

	"github.com/ganeshrvel/usb"
)

func candidateFromDeviceDescriptor(d *usb.Device) *Device {
	dd, err := d.GetDeviceDescriptor()
	if err != nil {
		return nil
	}
	for i := byte(0); i < dd.NumConfigurations; i++ {
		cdecs, err := d.GetConfigDescriptor(i)
		if err != nil {
			return nil
		}
		for _, iface := range cdecs.Interfaces {
			for _, a := range iface.AltSetting {
				if len(a.EndPoints) != 3 {
					continue
				}
				m := Device{}
				for _, s := range a.EndPoints {
					switch {
					case s.Direction() == usb.ENDPOINT_IN && s.TransferType() == usb.TRANSFER_TYPE_INTERRUPT:
						m.eventEP = s.EndpointAddress
					case s.Direction() == usb.ENDPOINT_IN && s.TransferType() == usb.TRANSFER_TYPE_BULK:
						m.fetchEP = s.EndpointAddress
					case s.Direction() == usb.ENDPOINT_OUT && s.TransferType() == usb.TRANSFER_TYPE_BULK:
						m.sendEP = s.EndpointAddress
					}
				}
				if m.sendEP > 0 && m.fetchEP > 0 && m.eventEP > 0 {
					m.devDescr = *dd
					m.ifaceDescr = a
					m.dev = d.Ref()
					m.configValue = cdecs.ConfigurationValue
					return &m
				}
			}
		}
	}

	return nil
}

// FindDevices finds likely MTP devices without opening them.
func FindDevices(c *usb.Context) ([]*Device, error) {
	l, err := c.GetDeviceList()
	if err != nil {
		return nil, err
	}

	var cands []*Device
	for _, d := range l {
		cand := candidateFromDeviceDescriptor(d)
		if cand != nil {
			cands = append(cands, cand)
		}
	}

	if len(l) > 0 {
		l.Done()
	}

	return cands, nil
}

// imageCaptureDevices finds cameras through IOKit without claiming USB.
func imageCaptureDevices() ([]imageCaptureTarget, error) {
	var message *C.char
	data := C.omtp_ic_devices(&message)
	defer C.free(unsafe.Pointer(data))
	if err := imageCaptureNativeError(message); err != nil {
		return nil, err
	}
	if data == nil {
		return nil, nil
	}
	var targets []imageCaptureTarget
	err := json.Unmarshal([]byte(C.GoString(data)), &targets)
	return targets, err
}

// selectDevice finds a device that matches given pattern
func selectDevice(cands []*Device, pattern string) (*Device, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}

	var found []*Device
	for _, cand := range cands {
		if err := cand.Open(); err != nil {
			continue
		}

		found = append(found, cand)
	}

	if len(found) == 0 {
		return nil, fmt.Errorf("no MTP devices found")
	}

	cands = found
	found = nil
	var ids []string
	for i, cand := range cands {
		id, err := cand.ID()
		if err != nil {
			// TODO - close cands
			return nil, fmt.Errorf("Id dev %d: %v", i, err)
		}

		if pattern == "" || re.FindString(id) != "" {
			found = append(found, cand)
			ids = append(ids, id)
		} else {
			cand.Close()
			cand.Done()
		}
	}

	if len(found) == 0 {
		return nil, fmt.Errorf("no device matched")
	}

	if len(found) > 1 {
		return nil, fmt.Errorf("mtp: more than 1 device: %s", strings.Join(ids, ","))
	}

	cand := found[0]
	config, err := cand.h.GetConfiguration()
	if err != nil {
		return nil, fmt.Errorf("could not get configuration of %v: %v",
			ids[0], err)
	}
	if config != cand.configValue {

		if err := cand.h.SetConfiguration(cand.configValue); err != nil {
			return nil, fmt.Errorf("could not set configuration of %v: %v",
				ids[0], err)
		}
	}
	return found[0], nil
}

// SelectDevice opens a matching MTP device, using ImageCaptureCore if Apple owns USB.
// GOMTPFS_FORCE_IMAGECAPTURE=1 forces the proxy. See RunMainLoop for CLI use.
func SelectDevice(pattern string) (*Device, error) {
	return SelectDeviceWithDebugging(pattern, false)
}

// SelectDeviceWithDebugging is SelectDevice with optional diagnostics.
func SelectDeviceWithDebugging(pattern string, allowDebugging bool) (*Device, error) {
	if d, err := selectImageCaptureDevice(pattern, allowDebugging); d != nil || err != nil {
		return d, err
	}
	d, err := selectUSBDevice(pattern, allowDebugging)
	if err == nil {
		return d, nil
	}
	// Apple may have claimed the interface during USB discovery.
	if d, sharedErr := selectImageCaptureDevice(pattern, allowDebugging); d != nil || sharedErr != nil {
		return d, sharedErr
	}
	return nil, err
}

func selectUSBDevice(pattern string, allowDebugging bool) (selected *Device, err error) {
	c := usb.NewContext()

	devs, err := FindDevices(c)
	if err != nil {
		return nil, err
	}
	if len(devs) == 0 {
		return nil, fmt.Errorf("no MTP devices found")
	}
	defer func() {
		for _, d := range devs {
			if d != selected {
				d.Close()
				d.Done()
			}
		}
	}()

	if allowDebugging {
		for _, _dev := range devs {
			_dev.USBDebug = allowDebugging
			_dev.DataDebug = allowDebugging
			_dev.MTPDebug = allowDebugging
		}
	}

	return selectDevice(devs, pattern)
}

func imageCaptureMatchingTarget(targets []imageCaptureTarget, pattern string, force bool) (*imageCaptureTarget, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	var selected *imageCaptureTarget
	matches := 0
	for i := range targets {
		id := strings.Join([]string{targets[i].Manufacturer, targets[i].Name, targets[i].Serial}, " ")
		if !re.MatchString(id) {
			continue
		}
		matches++
		if force || targets[i].AppleOwned {
			selected = &targets[i]
		}
	}
	if selected != nil && matches > 1 {
		return nil, fmt.Errorf("mtp: more than 1 device; connect one phone at a time")
	}
	if selected == nil && force {
		return nil, fmt.Errorf("imagecapture: no matching MTP camera interface found")
	}
	return selected, nil
}

func selectImageCaptureDevice(pattern string, debug bool) (*Device, error) {
	targets, err := imageCaptureDevices()
	if err != nil {
		return nil, err
	}
	force := os.Getenv("GOMTPFS_FORCE_IMAGECAPTURE") == "1"
	target, err := imageCaptureMatchingTarget(targets, pattern, force)
	if err != nil || target == nil {
		return nil, err
	}
	device := &Device{proxy: &ptpProxy{
		target: target,
		info: UsbDeviceInfo{
			IdVendor: target.Vendor, IdProduct: target.Product, Device: target.Release,
			Manufacturer: target.Manufacturer, Product: target.Name, SerialNumber: target.Serial,
		},
	}, MTPDebug: debug}
	if err := device.Open(); err != nil {
		return nil, err
	}
	if force {
		log.Printf("MTP transport: ImageCaptureCore (forced)")
	}
	return device, nil
}

type imageCaptureTarget struct {
	Vendor       uint16 `json:"vendor"`
	Product      uint16 `json:"product"`
	Location     uint32 `json:"location"`
	Release      uint16 `json:"release"`
	Manufacturer string `json:"manufacturer"`
	Name         string `json:"name"`
	Serial       string `json:"serial"`
	AppleOwned   bool   `json:"appleOwned"`
}
