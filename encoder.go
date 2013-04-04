package dbus

import (
	"bytes"
	"encoding/binary"
	"io"
	"reflect"
)

// An Encoder encodes values to the D-Bus wire format.
//
// The following types are directly encoded as their respective D-Bus
// equivalents:
//
//     Go type     | D-Bus type
//     ------------+-----------
//     byte        | BYTE
//     bool        | BOOLEAN
//     int16       | INT16
//     uint16      | UINT16
//     int32       | INT32
//     uint32      | UINT32
//     int64       | INT64
//     uint64      | UINT64
//     float64     | DOUBLE
//     string      | STRING
//     ObjectPath  | OBJECT_PATH
//     Signature   | SIGNATURE
//     Variant     | VARIANT
//     UnixFDIndex | UNIX_FD
//
// Slices and arrays encode as ARRAYs of their element type.
//
// Maps encode as DICTs, provided that their key type is a basic type.
//
// Structs other than Variant and Signature encode as a STRUCT containing its
// exported fields. Fields whose tag contains `dbus:"-"` and unexported field
// will be skipped.
//
// Pointers encode as the value they're pointed to.
//
// Trying to encode any other type (including int and uint) or a slice, map or
// struct containing an unsupported type will result in a panic.
type Encoder struct {
	out   io.Writer
	order binary.ByteOrder
	pos   int
}

// NewEncoder returns a new encoder that writes to out in the given byte order.
func NewEncoder(out io.Writer, order binary.ByteOrder) *Encoder {
	enc := new(Encoder)
	enc.out = out
	enc.order = order
	return enc
}

// Aligns the next output to be on a multiple of n. Panics on write errors.
func (enc *Encoder) align(n int) {
	if enc.pos%n != 0 {
		newpos := (enc.pos + n - 1) & ^(n - 1)
		empty := make([]byte, newpos-enc.pos)
		if _, err := enc.out.Write(empty); err != nil {
			panic(err)
		}
		enc.pos = newpos
	}
}

// Calls binary.Write(enc.out, enc.order, v) and panics on write errors.
func (enc *Encoder) binwrite(v interface{}) {
	if err := binary.Write(enc.out, enc.order, v); err != nil {
		panic(err)
	}
}

// Encode encodes a single value to the underyling reader. All written values
// are aligned properly as required by the DBus spec.
func (enc *Encoder) Encode(v interface{}) (err error) {
	defer func() {
		err, ok := recover().(error)
		if ok {
			// invalidTypeErrors are errors in the program and can't really be
			// recovered from
			if _, ok := err.(invalidTypeError); ok {
				panic(err)
			}
		}
	}()
	enc.encode(reflect.ValueOf(v), 0)
	return nil
}

// Encode is a shorthand for multiple Encode calls.
func (enc *Encoder) EncodeMulti(vs ...interface{}) error {
	for _, v := range vs {
		if err := enc.Encode(v); err != nil {
			return err
		}
	}
	return nil
}

// encode encodes the given value to the writer and panics on error. depth holds
// the depth of the container nesting.
func (enc *Encoder) encode(v reflect.Value, depth int) {
	enc.align(alignment(v.Type()))
	switch v.Kind() {
	case reflect.Uint8:
		var b [1]byte
		b[0] = byte(v.Uint())
		if _, err := enc.out.Write(b[:]); err != nil {
			panic(err)
		}
		enc.pos++
	case reflect.Bool:
		if v.Bool() {
			enc.encode(reflect.ValueOf(uint32(1)), depth)
		} else {
			enc.encode(reflect.ValueOf(uint32(0)), depth)
		}
	case reflect.Int16:
		enc.binwrite(int16(v.Int()))
		enc.pos += 2
	case reflect.Uint16:
		enc.binwrite(uint16(v.Uint()))
		enc.pos += 2
	case reflect.Int32:
		enc.binwrite(int32(v.Int()))
		enc.pos += 4
	case reflect.Uint32:
		enc.binwrite(uint32(v.Uint()))
		enc.pos += 4
	case reflect.Int64:
		enc.binwrite(v.Int())
		enc.pos += 8
	case reflect.Uint64:
		enc.binwrite(v.Uint())
		enc.pos += 8
	case reflect.Float64:
		enc.binwrite(v.Float())
		enc.pos += 8
	case reflect.String:
		enc.encode(reflect.ValueOf(uint32(len(v.String()))), depth)
		b := make([]byte, v.Len()+1)
		copy(b, v.String())
		b[len(b)-1] = 0
		n, err := enc.out.Write(b)
		if err != nil {
			panic(err)
		}
		enc.pos += n
	case reflect.Ptr:
		enc.encode(v.Elem(), depth)
	case reflect.Slice, reflect.Array:
		if depth >= 64 {
			panic(FormatError("input exceeds container depth limit"))
		}
		var buf bytes.Buffer
		bufenc := NewEncoder(&buf, enc.order)

		for i := 0; i < v.Len(); i++ {
			bufenc.encode(v.Index(i), depth+1)
		}
		enc.encode(reflect.ValueOf(uint32(buf.Len())), depth)
		length := buf.Len()
		enc.align(alignment(v.Type().Elem()))
		if _, err := buf.WriteTo(enc.out); err != nil {
			panic(err)
		}
		enc.pos += length
	case reflect.Struct:
		if depth >= 64 && v.Type() != signatureType {
			panic(FormatError("input exceeds container depth limit"))
		}
		switch t := v.Type(); t {
		case signatureType:
			str := v.Field(0)
			enc.encode(reflect.ValueOf(byte(str.Len())), depth+1)
			b := make([]byte, str.Len()+1)
			copy(b, str.String())
			b[len(b)-1] = 0
			n, err := enc.out.Write(b)
			if err != nil {
				panic(err)
			}
			enc.pos += n
		case variantType:
			variant := v.Interface().(Variant)
			enc.encode(reflect.ValueOf(variant.sig), depth+1)
			enc.encode(reflect.ValueOf(variant.value), depth+1)
		default:
			for i := 0; i < v.Type().NumField(); i++ {
				field := t.Field(i)
				if field.PkgPath == "" && field.Tag.Get("dbus") != "-" {
					enc.encode(v.Field(i), depth+1)
				}
			}
		}
	case reflect.Map:
		// Maps are arrays of structures, so they actually increase the depth by
		// 2.
		if depth >= 63 {
			panic(FormatError("input exceeds container depth limit"))
		}
		if !isKeyType(v.Type().Key()) {
			panic(invalidTypeError{v.Type()})
		}
		keys := v.MapKeys()
		var buf bytes.Buffer
		bufenc := NewEncoder(&buf, enc.order)
		for _, k := range keys {
			bufenc.align(8)
			bufenc.encode(k, depth+2)
			bufenc.encode(v.MapIndex(k), depth+2)
		}
		enc.encode(reflect.ValueOf(uint32(buf.Len())), depth)
		length := buf.Len()
		enc.align(8)
		if _, err := buf.WriteTo(enc.out); err != nil {
			panic(err)
		}
		enc.pos += length
	default:
		panic(invalidTypeError{v.Type()})
	}
}
