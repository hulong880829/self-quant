package aggdata

import (
	"encoding/binary"
	"testing"
)

func TestDecodeGatewayBBO(t *testing.T) {
	payload := validGatewayBBOPayload()
	frame := gatewayFrameForTest(KindBBO, 1, 7, payload)
	decoded, err := DecodeGatewayFrame(frame)
	if err != nil {
		t.Fatalf("DecodeGatewayFrame: %v", err)
	}
	if decoded.TopicID != 7 || decoded.BBO.Base != "BTC" ||
		decoded.BBO.Quote != "USDT" || decoded.BBO.GatedBid.Price != 100 {
		t.Fatalf("unexpected decoded frame: %+v", decoded)
	}
}

func TestDecodeGatewayRejectsMalformed(t *testing.T) {
	valid := gatewayFrameForTest(KindBBO, 1, 1, validGatewayBBOPayload())
	tests := [][]byte{
		valid[:51],
		append([]byte(nil), valid...),
		append([]byte(nil), valid...),
	}
	tests[1][0] = 0
	binary.LittleEndian.PutUint32(tests[2][16:20], 1)
	for index, test := range tests {
		if _, err := DecodeGatewayFrame(test); err == nil {
			t.Fatalf("case %d unexpectedly succeeded", index)
		}
	}
}

func TestDecodeGatewayBookOrdering(t *testing.T) {
	payload := make([]byte, 69+2*85)
	binary.LittleEndian.PutUint64(payload[0:8], 123)
	copy(payload[14:30], "BTC")
	copy(payload[30:46], "USDT")
	payload[46] = 1
	payload[54], payload[55], payload[56] = 2, 3, 1
	binary.LittleEndian.PutUint32(payload[57:61], 1)
	binary.LittleEndian.PutUint32(payload[61:65], 1)
	binary.LittleEndian.PutUint16(payload[65:67], 1)
	binary.LittleEndian.PutUint16(payload[67:69], 1)
	putCompactLevel(payload[69:154], 100, 5)
	putCompactLevel(payload[154:239], 101, 6)
	frame := gatewayFrameForTest(KindBook, 2, 2, payload)
	decoded, err := DecodeGatewayFrame(frame)
	if err != nil {
		t.Fatalf("DecodeGatewayFrame: %v", err)
	}
	if len(decoded.Book.Bids) != 1 || decoded.Book.Asks[0].Price != 101 {
		t.Fatalf("unexpected book: %+v", decoded.Book)
	}
}

func gatewayFrameForTest(kind Kind, format uint8, topic uint16, payload []byte) []byte {
	frame := make([]byte, gatewayHeaderSize+len(payload))
	binary.LittleEndian.PutUint32(frame[0:4], gatewayMagic)
	binary.LittleEndian.PutUint16(frame[4:6], 1)
	frame[8], frame[9] = byte(kind), format
	binary.LittleEndian.PutUint16(frame[10:12], topic)
	binary.LittleEndian.PutUint16(frame[14:16], gatewayHeaderSize)
	binary.LittleEndian.PutUint32(frame[16:20], uint32(len(payload)))
	binary.LittleEndian.PutUint64(frame[20:28], 1)
	binary.LittleEndian.PutUint64(frame[28:36], 2)
	binary.LittleEndian.PutUint64(frame[36:44], 3)
	binary.LittleEndian.PutUint64(frame[44:52], 4)
	copy(frame[gatewayHeaderSize:], payload)
	return frame
}

func validGatewayBBOPayload() []byte {
	payload := make([]byte, sqmdAggBboRecordSize)
	binary.LittleEndian.PutUint32(payload[0:4], sqmdMagic)
	binary.LittleEndian.PutUint16(payload[4:6], sqmdSchemaMajor)
	binary.LittleEndian.PutUint16(payload[6:8], sqmdSchemaMinor)
	binary.LittleEndian.PutUint16(payload[8:10], sqmdAggBboMessage)
	binary.LittleEndian.PutUint16(payload[10:12], sqmdAggBboRecordSize)
	putABISide(payload[72:176], 100, 5)
	putABISide(payload[176:280], 101, 6)
	putRawSide(payload[280:304], 100, 5)
	putRawSide(payload[304:328], 101, 6)
	copy(payload[328:344], "BTC")
	copy(payload[344:360], "USDT")
	payload[360], payload[368], payload[369], payload[370] = 1, 2, 3, 1
	binary.LittleEndian.PutUint32(payload[376:380], 1)
	binary.LittleEndian.PutUint32(payload[380:384], 1)
	return payload
}

func putABISide(output []byte, price, quantity int64) {
	binary.LittleEndian.PutUint64(output[0:8], uint64(price))
	binary.LittleEndian.PutUint64(output[8:16], uint64(quantity))
	binary.LittleEndian.PutUint64(output[16:24], uint64(quantity))
	binary.LittleEndian.PutUint32(output[88:92], 1)
	output[98] = 1
}

func putRawSide(output []byte, price, quantity int64) {
	binary.LittleEndian.PutUint64(output[0:8], uint64(price))
	binary.LittleEndian.PutUint64(output[8:16], uint64(quantity))
	binary.LittleEndian.PutUint32(output[16:20], 1)
}

func putCompactLevel(output []byte, price, quantity int64) {
	binary.LittleEndian.PutUint64(output[0:8], uint64(price))
	binary.LittleEndian.PutUint64(output[8:16], uint64(quantity))
	binary.LittleEndian.PutUint64(output[16:24], uint64(quantity))
	binary.LittleEndian.PutUint32(output[80:84], 1)
	output[84] = 1
}
