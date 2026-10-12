package mtp

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"math/rand"
	"time"
)

func init() {
	rand.Seed(time.Now().UnixNano())
}

// OpenSession opens a session, which is necesary for any command that
// queries or modifies storage. It is an error to open a session
// twice.  If OpenSession() fails, it will not attempt to close the device.
// Proxy sessions are opened by Device.Open.
func (d *Device) OpenSession() error {
	if d.proxy != nil {
		if d.proxy.wire == nil {
			return fmt.Errorf("mtp: device is not open")
		}
		return nil
	}
	if d.session != nil {
		return fmt.Errorf("session already open")
	}
	var req, rep Container
	req.Code = OC_OpenSession

	// avoid 0xFFFFFFFF and 0x00000000 for session IDs.
	sid := uint32(rand.Int31()) | 1
	req.Param = []uint32{sid} // session

	// If opening the session fails, we want to be able to reset
	// the device, so don't do sanity checks afterwards.
	if err := d.runTransaction(&req, &rep, nil, nil, 0, EmptyProgressFunc); err != nil {
		return err
	}

	d.session = &sessionData{
		tid: 1,
		sid: sid,
	}
	return nil
}

// Closes a sessions. This is done automatically if the device is closed.
func (d *Device) CloseSession() error {
	if d.proxy != nil {
		return d.Close()
	}
	var req, rep Container
	req.Code = OC_CloseSession
	err := d.RunTransaction(&req, &rep, nil, nil, 0, EmptyProgressFunc)
	d.session = nil
	return err
}

func (d *Device) GetData(req *Container, info interface{}) error {
	var buf bytes.Buffer
	var rep Container
	if err := d.RunTransaction(req, &rep, &buf, nil, 0, EmptyProgressFunc); err != nil {
		return err
	}
	err := Decode(&buf, info)
	if d.MTPDebug && err == nil {
		log.Printf("MTP decoded %#v", info)
	}
	return err
}

func (d *Device) GetDeviceInfo(info *DeviceInfo) error {
	var req Container
	req.Code = OC_GetDeviceInfo
	return d.GetData(&req, info)
}

func (d *Device) GetStorageIDs(info *Uint32Array) error {
	var req Container
	req.Code = OC_GetStorageIDs
	return d.GetData(&req, info)
}

func (d *Device) GetObjectPropDesc(objPropCode, objFormatCode uint16, info *ObjectPropDesc) error {
	var req Container
	req.Code = OC_MTP_GetObjectPropDesc
	req.Param = []uint32{uint32(objPropCode), uint32(objFormatCode)}
	return d.GetData(&req, info)
}

func (d *Device) GetObjectPropValue(objHandle uint32, objPropCode uint16, value interface{}) error {
	var req Container
	req.Code = OC_MTP_GetObjectPropValue
	req.Param = []uint32{objHandle, uint32(objPropCode)}
	return d.GetData(&req, value)
}

func (d *Device) SetObjectPropValue(objHandle uint32, objPropCode uint16, value interface{}) error {
	var req, rep Container
	req.Code = OC_MTP_SetObjectPropValue
	req.Param = []uint32{objHandle, uint32(objPropCode)}
	return d.SendData(&req, &rep, value)
}

func (d *Device) SendData(req *Container, rep *Container, value interface{}) error {
	var buf bytes.Buffer
	if err := Encode(&buf, value); err != nil {
		return err
	}
	if d.MTPDebug {
		log.Printf("MTP encoded %#v", value)
	}
	return d.RunTransaction(req, rep, nil, &buf, int64(buf.Len()), EmptyProgressFunc)
}

func (d *Device) GetObjectPropsSupported(objFormatCode uint16, props *Uint16Array) error {
	var req Container

	req.Code = OC_MTP_GetObjectPropsSupported
	req.Param = []uint32{uint32(objFormatCode)}
	return d.GetData(&req, props)
}

func (d *Device) GetDevicePropDesc(propCode uint16, info *DevicePropDesc) error {
	var req Container
	req.Code = OC_GetDevicePropDesc
	req.Param = append(req.Param, uint32(propCode))
	return d.GetData(&req, info)
}

func (d *Device) SetDevicePropValue(propCode uint32, src interface{}) error {
	var req, rep Container
	req.Code = OC_SetDevicePropValue
	req.Param = []uint32{propCode}
	return d.SendData(&req, &rep, src)
}

func (d *Device) GetDevicePropValue(propCode uint32, dest interface{}) error {
	var req Container
	req.Code = OC_GetDevicePropValue
	req.Param = []uint32{propCode}
	return d.GetData(&req, dest)
}

func (d *Device) ResetDevicePropValue(propCode uint32) error {
	var req, rep Container
	req.Code = OC_ResetDevicePropValue
	req.Param = []uint32{propCode}
	return d.RunTransaction(&req, &rep, nil, nil, 0, EmptyProgressFunc)
}

func (d *Device) GetStorageInfo(ID uint32, info *StorageInfo) error {
	var req Container
	req.Code = OC_GetStorageInfo
	req.Param = []uint32{ID}
	return d.GetData(&req, info)
}

func (d *Device) GetObjectHandles(storageID, objFormatCode, parent uint32, info *Uint32Array) error {
	var req Container
	req.Code = OC_GetObjectHandles
	req.Param = []uint32{storageID, objFormatCode, parent}
	return d.GetData(&req, info)
}

func (d *Device) GetObjectInfo(handle uint32, info *ObjectInfo) error {
	var req Container
	req.Code = OC_GetObjectInfo
	req.Param = []uint32{handle}
	return d.GetData(&req, info)
}

func (d *Device) GetNumObjects(storageId uint32, formatCode uint16, parent uint32) (uint32, error) {
	var req, rep Container
	req.Code = OC_GetNumObjects
	req.Param = []uint32{storageId, uint32(formatCode), parent}
	if err := d.RunTransaction(&req, &rep, nil, nil, 0, EmptyProgressFunc); err != nil {
		return 0, err
	}
	return rep.Param[0], nil
}

func (d *Device) DeleteObject(handle uint32) error {
	var req, rep Container
	req.Code = OC_DeleteObject
	req.Param = []uint32{handle, 0x0}

	return d.RunTransaction(&req, &rep, nil, nil, 0, EmptyProgressFunc)
}

func (d *Device) SendObjectInfo(wantStorageID, wantParent uint32, info *ObjectInfo) (storageID, parent, handle uint32, err error) {
	var req, rep Container
	req.Code = OC_SendObjectInfo
	req.Param = []uint32{wantStorageID, wantParent}

	originalSize := info.CompressedSize
	if d.proxy != nil && info.ObjectFormat != OFC_Association && (originalSize == 0 || originalSize > proxyChunkSize) {
		if !d.canEditObjects() {
			return 0, 0, 0, fmt.Errorf("mtp: this phone needs Android object-edit support for empty files or uploads over 8 MiB")
		}
		// Start small, then extend/truncate. Empty proxy payloads skip USB data-out.
		initial := *info
		initial.CompressedSize = proxyChunkSize
		if originalSize == 0 {
			initial.CompressedSize = 1
		}
		info = &initial
	}

	if err = d.SendData(&req, &rep, info); err != nil {
		return
	}

	if len(rep.Param) < 3 {
		err = fmt.Errorf("SendObjectInfo: got %v, need 3 response parameters", rep.Param)
		if d.proxy != nil {
			d.Close()
		}
		return
	}
	if d.proxy != nil && info.ObjectFormat != OFC_Association {
		d.proxy.pending = &pendingUpload{handle: rep.Param[2], size: originalSize}
	}
	return rep.Param[0], rep.Param[1], rep.Param[2], nil
}

func (d *Device) SendObject(r io.Reader, size int64, progressCb ProgressFunc) (err error) {
	var req, rep Container
	req.Code = OC_SendObject
	if d.proxy == nil {
		return d.RunTransaction(&req, &rep, nil, r, size, progressCb)
	}
	if progressCb == nil {
		progressCb = EmptyProgressFunc
	}
	pending := d.proxy.pending
	if pending == nil {
		return fmt.Errorf("mtp: SendObject requires a preceding SendObjectInfo")
	}
	d.proxy.pending = nil
	if r == nil || size < 0 || (pending.size != 0xffffffff && int64(pending.size) != size) ||
		(pending.size == 0xffffffff && size < 0xffffffff) {
		d.Close()
		return fmt.Errorf("mtp: upload size does not match SendObjectInfo")
	}
	initial := size
	if initial > proxyChunkSize {
		initial = proxyChunkSize
	} else if initial == 0 {
		initial, r = 1, bytes.NewReader([]byte{0})
	}
	if err := d.RunTransaction(&req, &rep, nil, r, initial, EmptyProgressFunc); err != nil {
		// Even a local read failure leaves the phone waiting for SendObject.
		d.Close()
		return err
	}
	if size > 0 && size <= proxyChunkSize {
		return progressCb(size)
	}
	if err := d.AndroidBeginEditObject(pending.handle); err != nil {
		return err
	}
	defer func() {
		// Finish the edit unless the transport has already failed.
		if d.proxy.wire == nil {
			return
		}
		endErr := d.AndroidEndEditObject(pending.handle)
		if err == nil {
			err = endErr
		}
		if err == nil {
			var actual Uint64Value
			err = d.GetObjectPropValue(pending.handle, OPC_ObjectSize, &actual)
			if err == nil && actual.Value != uint64(size) {
				err = fmt.Errorf("mtp: committed upload size is %d, expected %d", actual.Value, size)
			}
			if err == nil {
				err = progressCb(size)
			}
		}
	}()
	if size == 0 {
		return d.AndroidTruncate(pending.handle, 0)
	}
	if err := progressCb(initial); err != nil {
		return err
	}
	for offset := initial; offset < size; {
		n := int64(proxyChunkSize)
		if n > size-offset {
			n = size - offset
		}
		if err := d.AndroidSendPartialObject(pending.handle, offset, uint32(n), r); err != nil {
			return err
		}
		offset += n
		if offset < size {
			if err := progressCb(offset); err != nil {
				return err
			}
		}
	}
	return nil
}

func (d *Device) GetObject(handle uint32, w io.Writer, progressCb ProgressFunc) error {
	var req, rep Container
	req.Code = OC_GetObject
	req.Param = []uint32{handle}
	if d.proxy == nil {
		return d.RunTransaction(&req, &rep, w, nil, 0, progressCb)
	}
	if w == nil {
		return fmt.Errorf("mtp: GetObject requires a destination")
	}
	if progressCb == nil {
		progressCb = EmptyProgressFunc
	}
	var objectSize Uint64Value
	if err := d.GetObjectPropValue(handle, OPC_ObjectSize, &objectSize); err != nil {
		return err
	}
	size := objectSize.Value
	if size > uint64(^uint64(0)>>1) {
		return fmt.Errorf("mtp: object is too large")
	}
	code := uint16(OC_GetPartialObject)
	if d.proxy.ops[OC_ANDROID_GET_PARTIAL_OBJECT64] {
		code = OC_ANDROID_GET_PARTIAL_OBJECT64
	} else if !d.proxy.ops[code] || size > 0xffffffff {
		if size > proxyMaxPayload {
			return fmt.Errorf("mtp: phone does not support partial reads for this file size")
		}
		var received int64
		if err := d.RunTransaction(&req, &rep, w, nil, 0, func(n int64) error { received = n; return nil }); err != nil {
			return err
		}
		if uint64(received) != size {
			return fmt.Errorf("mtp: incomplete object download")
		}
		return progressCb(received)
	}
	for offset := uint64(0); offset < size; {
		n := uint32(proxyChunkSize)
		if uint64(n) > size-offset {
			n = uint32(size - offset)
		}
		req.Code = code
		req.Param = []uint32{handle, uint32(offset), n}
		if code == OC_ANDROID_GET_PARTIAL_OBJECT64 {
			req.Param = []uint32{handle, uint32(offset), uint32(offset >> 32), n}
		}
		var received int64
		if err := d.RunTransaction(&req, &rep, w, nil, 0, func(n int64) error { received = n; return nil }); err != nil {
			return err
		}
		if len(rep.Param) != 1 || rep.Param[0] != n || received != int64(n) {
			d.Close()
			return fmt.Errorf("mtp: incomplete partial-object download")
		}
		offset += uint64(n)
		if err := progressCb(int64(offset)); err != nil {
			return err
		}
	}
	return nil
}

func (d *Device) GetPartialObject(handle uint32, w io.Writer, offset uint32, size uint32) error {
	var req, rep Container
	req.Code = OC_ANDROID_GET_PARTIAL_OBJECT64
	req.Param = []uint32{handle, offset, size}
	return d.RunTransaction(&req, &rep, w, nil, 0, EmptyProgressFunc)
}
