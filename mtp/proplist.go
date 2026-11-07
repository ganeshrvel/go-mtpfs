package mtp

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"reflect"
)

// ObjectPropListElement is one (object, property, value) entry of an MTP ObjectPropList dataset.
type ObjectPropListElement struct {
	Handle   uint32
	PropCode uint16
	DataType uint16
	Value    interface{}
}

// GetObjectPropList fetches [propCode] (0xFFFFFFFF = all properties) of [handle] in a single transaction.
// With [depth] 1 it returns the property of every direct child of [handle] instead, which turns a
// directory listing from one round trip per object into one round trip per directory.
// Devices may reject depth or property specifications they don't support (RCError); callers should fall
// back to GetObjectInfo per object.
func (d *Device) GetObjectPropList(handle, formatCode, propCode, depth uint32) ([]ObjectPropListElement, error) {
	var req, rep Container
	req.Code = OC_MTP_GetObjPropList
	req.Param = []uint32{handle, formatCode, propCode, 0, depth}

	var buf bytes.Buffer
	if err := d.RunTransaction(&req, &rep, &buf, nil, 0, EmptyProgressFunc); err != nil {
		return nil, err
	}

	return decodeObjectPropList(&buf)
}

func decodeObjectPropList(r io.Reader) ([]ObjectPropListElement, error) {
	var n uint32
	if err := binary.Read(r, byteOrder, &n); err != nil {
		return nil, err
	}

	var out []ObjectPropListElement
	for i := uint32(0); i < n; i++ {
		var hdr struct {
			Handle             uint32
			PropCode, DataType uint16
		}
		if err := binary.Read(r, byteOrder, &hdr); err != nil {
			return nil, err
		}

		v, err := decodePropValue(r, hdr.DataType)
		if err != nil {
			return nil, fmt.Errorf("decoding prop 0x%x of handle %d: %w", hdr.PropCode, hdr.Handle, err)
		}

		out = append(out, ObjectPropListElement{hdr.Handle, hdr.PropCode, hdr.DataType, v})
	}

	return out, nil
}

// ponytail: array data types are not decoded (none of the file listing properties use them); add when needed.
func decodePropValue(r io.Reader, dataType uint16) (interface{}, error) {
	var v interface{}
	switch dataType {
	case DTC_STR:
		return decodeStr(r)
	case DTC_INT128, DTC_UINT128:
		var b [16]byte
		_, err := io.ReadFull(r, b[:])
		return b, err
	case DTC_INT8:
		v = new(int8)
	case DTC_UINT8:
		v = new(uint8)
	case DTC_INT16:
		v = new(int16)
	case DTC_UINT16:
		v = new(uint16)
	case DTC_INT32:
		v = new(int32)
	case DTC_UINT32:
		v = new(uint32)
	case DTC_INT64:
		v = new(int64)
	case DTC_UINT64:
		v = new(uint64)
	default:
		return nil, fmt.Errorf("unsupported data type 0x%x", dataType)
	}

	if err := binary.Read(r, byteOrder, v); err != nil {
		return nil, err
	}

	return reflect.ValueOf(v).Elem().Interface(), nil
}
