package main

import (
	"encoding/binary"
	"fmt"
	"math"
	"time"
)

type protoWireType uint64

const (
	protoWireVarint      protoWireType = 0
	protoWireFixed64     protoWireType = 1
	protoWireLengthDelim protoWireType = 2
	protoWireFixed32     protoWireType = 5
)

type protoReader struct {
	data []byte
	pos  int
}

func newProtoReader(data []byte) *protoReader {
	return &protoReader{data: data}
}

func (r *protoReader) nextField() (fieldNumber int, wireType protoWireType, ok bool, err error) {
	if r.pos >= len(r.data) {
		return 0, 0, false, nil
	}
	key, n, err := readVarint(r.data[r.pos:])
	if err != nil {
		return 0, 0, false, err
	}
	r.pos += n
	fieldNumber = int(key >> 3)
	wireType = protoWireType(key & 0x07)
	if wireType > 5 || wireType == 3 || wireType == 4 {
		return 0, 0, false, fmt.Errorf("unsupported wire type %d", wireType)
	}
	return fieldNumber, wireType, true, nil
}

func (r *protoReader) skipField(wireType protoWireType) error {
	switch wireType {
	case protoWireVarint:
		_, n, err := readVarint(r.data[r.pos:])
		if err != nil {
			return err
		}
		r.pos += n
	case protoWireFixed64:
		if r.pos+8 > len(r.data) {
			return fmt.Errorf("truncated fixed64")
		}
		r.pos += 8
	case protoWireLengthDelim:
		length, n, err := readVarint(r.data[r.pos:])
		if err != nil {
			return err
		}
		r.pos += n
		if r.pos+int(length) > len(r.data) {
			return fmt.Errorf("truncated length-delimited field")
		}
		r.pos += int(length)
	case protoWireFixed32:
		if r.pos+4 > len(r.data) {
			return fmt.Errorf("truncated fixed32")
		}
		r.pos += 4
	default:
		return fmt.Errorf("unsupported wire type %d", wireType)
	}
	return nil
}

func (r *protoReader) readVarintField() (uint64, error) {
	value, n, err := readVarint(r.data[r.pos:])
	if err != nil {
		return 0, err
	}
	r.pos += n
	return value, nil
}

func (r *protoReader) readLengthDelimited() ([]byte, error) {
	length, n, err := readVarint(r.data[r.pos:])
	if err != nil {
		return nil, err
	}
	r.pos += n
	end := r.pos + int(length)
	if end > len(r.data) {
		return nil, fmt.Errorf("truncated length-delimited payload")
	}
	chunk := r.data[r.pos:end]
	r.pos = end
	return chunk, nil
}

func (r *protoReader) readString() (string, error) {
	data, err := r.readLengthDelimited()
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func readVarint(data []byte) (uint64, int, error) {
	var value uint64
	var shift uint
	for i, b := range data {
		value |= uint64(b&0x7F) << shift
		if b < 0x80 {
			return value, i + 1, nil
		}
		shift += 7
		if shift > 63 {
			break
		}
	}
	return 0, 0, fmt.Errorf("truncated varint")
}

func appendProtoVarint(buf []byte, value uint64) []byte {
	for value >= 0x80 {
		buf = append(buf, byte((value&0x7F)|0x80))
		value >>= 7
	}
	return append(buf, byte(value))
}

func appendProtoFieldKey(buf []byte, fieldNumber int, wireType protoWireType) []byte {
	key := uint64(fieldNumber<<3) | uint64(wireType)
	return appendProtoVarint(buf, key)
}

func appendProtoString(buf []byte, fieldNumber int, value string) []byte {
	buf = appendProtoFieldKey(buf, fieldNumber, protoWireLengthDelim)
	encoded := []byte(value)
	buf = appendProtoVarint(buf, uint64(len(encoded)))
	return append(buf, encoded...)
}

func appendProtoBool(buf []byte, fieldNumber int, value bool) []byte {
	buf = appendProtoFieldKey(buf, fieldNumber, protoWireVarint)
	if value {
		return appendProtoVarint(buf, 1)
	}
	return appendProtoVarint(buf, 0)
}

type windsurfPlanStatus struct {
	PlanName                    string
	DailyQuotaRemainingPercent  int
	WeeklyQuotaRemainingPercent int
	DailyResetAt                time.Time
	WeeklyResetAt               time.Time
	PlanEnd                     time.Time
}

func windsurfEncodePlanStatusRequest(authToken string) []byte {
	var buf []byte
	buf = appendProtoString(buf, 1, authToken)
	buf = appendProtoBool(buf, 2, true)
	return buf
}

func windsurfDecodePlanStatusResponse(data []byte) (windsurfPlanStatus, error) {
	reader := newProtoReader(data)
	var status windsurfPlanStatus
	for {
		field, wire, ok, err := reader.nextField()
		if err != nil {
			return status, err
		}
		if !ok {
			break
		}
		if field == 1 && wire == protoWireLengthDelim {
			chunk, err := reader.readLengthDelimited()
			if err != nil {
				return status, err
			}
			parsed, err := windsurfDecodePlanStatus(chunk)
			if err != nil {
				return status, err
			}
			status = parsed
			continue
		}
		if err := reader.skipField(wire); err != nil {
			return status, err
		}
	}
	return status, nil
}

func windsurfDecodePlanStatus(data []byte) (windsurfPlanStatus, error) {
	reader := newProtoReader(data)
	var status windsurfPlanStatus
	for {
		field, wire, ok, err := reader.nextField()
		if err != nil {
			return status, err
		}
		if !ok {
			break
		}
		switch field {
		case 1:
			if wire != protoWireLengthDelim {
				if err := reader.skipField(wire); err != nil {
					return status, err
				}
				continue
			}
			chunk, err := reader.readLengthDelimited()
			if err != nil {
				return status, err
			}
			name, err := windsurfDecodePlanInfo(chunk)
			if err != nil {
				return status, err
			}
			status.PlanName = name
		case 3:
			if wire != protoWireLengthDelim {
				if err := reader.skipField(wire); err != nil {
					return status, err
				}
				continue
			}
			chunk, err := reader.readLengthDelimited()
			if err != nil {
				return status, err
			}
			status.PlanEnd = windsurfDecodeTimestamp(chunk)
		case 14:
			if wire == protoWireVarint {
				value, err := reader.readVarintField()
				if err != nil {
					return status, err
				}
				status.DailyQuotaRemainingPercent = int(value)
			} else if err := reader.skipField(wire); err != nil {
				return status, err
			}
		case 15:
			if wire == protoWireVarint {
				value, err := reader.readVarintField()
				if err != nil {
					return status, err
				}
				status.WeeklyQuotaRemainingPercent = int(value)
			} else if err := reader.skipField(wire); err != nil {
				return status, err
			}
		case 17:
			if wire == protoWireVarint {
				value, err := reader.readVarintField()
				if err != nil {
					return status, err
				}
				status.DailyResetAt = time.Unix(int64(value), 0).UTC()
			} else if err := reader.skipField(wire); err != nil {
				return status, err
			}
		case 18:
			if wire == protoWireVarint {
				value, err := reader.readVarintField()
				if err != nil {
					return status, err
				}
				status.WeeklyResetAt = time.Unix(int64(value), 0).UTC()
			} else if err := reader.skipField(wire); err != nil {
				return status, err
			}
		default:
			if err := reader.skipField(wire); err != nil {
				return status, err
			}
		}
	}
	return status, nil
}

func windsurfDecodePlanInfo(data []byte) (string, error) {
	reader := newProtoReader(data)
	var planName string
	for {
		field, wire, ok, err := reader.nextField()
		if err != nil {
			return "", err
		}
		if !ok {
			break
		}
		if field == 2 && wire == protoWireLengthDelim {
			planName, err = reader.readString()
			if err != nil {
				return "", err
			}
			continue
		}
		if err := reader.skipField(wire); err != nil {
			return "", err
		}
	}
	return planName, nil
}

func windsurfDecodeTimestamp(data []byte) time.Time {
	reader := newProtoReader(data)
	var seconds int64
	var nanos int32
	for {
		field, wire, ok, err := reader.nextField()
		if err != nil || !ok {
			break
		}
		switch field {
		case 1:
			if wire == protoWireVarint {
				value, err := reader.readVarintField()
				if err == nil {
					seconds = int64(value)
				}
			} else if err := reader.skipField(wire); err != nil {
				return time.Time{}
			}
		case 2:
			if wire == protoWireVarint {
				value, err := reader.readVarintField()
				if err == nil {
					nanos = int32(value)
				}
			} else if err := reader.skipField(wire); err != nil {
				return time.Time{}
			}
		default:
			if err := reader.skipField(wire); err != nil {
				return time.Time{}
			}
		}
	}
	return time.Unix(seconds, int64(nanos)).UTC()
}

type grokBillingSnapshot struct {
	UsedPercent float64
	ResetAt     time.Time
}

type protoFixedField struct {
	path  []uint64
	value float32
	order int
}

type protoVarintField struct {
	path  []uint64
	value uint64
}

func grokGRPCWebDataFrames(data []byte) [][]byte {
	var frames [][]byte
	index := 0
	for index+5 <= len(data) {
		flags := data[index]
		length := int(data[index+1])<<24 | int(data[index+2])<<16 | int(data[index+3])<<8 | int(data[index+4])
		start := index + 5
		end := start + length
		if length < 0 || end > len(data) {
			return frames
		}
		if flags&0x80 == 0 {
			frames = append(frames, data[start:end])
		}
		index = end
	}
	return frames
}

func grokParseBillingResponse(data []byte, now time.Time) (grokBillingSnapshot, error) {
	payloads := grokGRPCWebDataFrames(data)
	if len(payloads) == 0 && len(data) > 0 && data[0]>>3 > 0 {
		payloads = [][]byte{data}
	}
	if len(payloads) == 0 {
		return grokBillingSnapshot{}, fmt.Errorf("empty grok billing response")
	}

	var fixedFields []protoFixedField
	var varintFields []protoVarintField
	order := 0
	for _, payload := range payloads {
		scanProtobuf(payload, nil, 0, &fixedFields, &varintFields, &order)
	}

	var usedPercent *float64
	for _, field := range fixedFields {
		if len(field.path) == 0 || field.path[len(field.path)-1] != 1 {
			continue
		}
		v := float64(field.value)
		if !math.IsNaN(v) && v >= 0 && v <= 100 {
			if usedPercent == nil || field.path[len(field.path)-1] == 1 {
				val := v
				usedPercent = &val
			}
		}
	}

	var resetAt time.Time
	for _, field := range varintFields {
		if field.value < 1_700_000_000 || field.value > 2_100_000_000 {
			continue
		}
		candidate := time.Unix(int64(field.value), 0).UTC()
		if candidate.After(now) {
			if resetAt.IsZero() || candidate.Before(resetAt) {
				resetAt = candidate
			}
		}
	}

	if usedPercent == nil {
		return grokBillingSnapshot{}, fmt.Errorf("could not parse grok billing usage")
	}
	return grokBillingSnapshot{UsedPercent: *usedPercent, ResetAt: resetAt}, nil
}

func scanProtobuf(data []byte, path []uint64, depth int, fixedFields *[]protoFixedField, varintFields *[]protoVarintField, order *int) {
	if depth > 8 {
		return
	}
	reader := newProtoReader(data)
	for {
		field, wire, ok, err := reader.nextField()
		if err != nil || !ok {
			return
		}
		currentPath := append(append([]uint64{}, path...), uint64(field))
		switch wire {
		case protoWireVarint:
			value, err := reader.readVarintField()
			if err != nil {
				return
			}
			*varintFields = append(*varintFields, protoVarintField{path: currentPath, value: value})
		case protoWireFixed32:
			if reader.pos+4 > len(reader.data) {
				return
			}
			raw := binary.LittleEndian.Uint32(reader.data[reader.pos : reader.pos+4])
			reader.pos += 4
			*fixedFields = append(*fixedFields, protoFixedField{
				path:  currentPath,
				value: math.Float32frombits(raw),
				order: *order,
			})
			*order++
		case protoWireLengthDelim:
			chunk, err := reader.readLengthDelimited()
			if err != nil {
				return
			}
			scanProtobuf(chunk, currentPath, depth+1, fixedFields, varintFields, order)
		default:
			if err := reader.skipField(wire); err != nil {
				return
			}
		}
	}
}
