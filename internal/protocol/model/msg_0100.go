package model

import (
	"strings"

	"github.com/fakeyanss/jt808-server-go/internal/codec/hex"
)

// 终端注册
type Msg0100 struct {
	Header         *MsgHeader `json:"header"`
	ProvinceID     uint16     `json:"provinceId"`     // 省域ID，GBT2260 行政区号6位前2位。
	CityID         uint16     `json:"cityId"`         // 市县域ID，GBT2260 行政区号6位后4位
	ManufacturerID string     `json:"manufacturerId"` // 制造商ID
	DeviceMode     string     `json:"deviceMode"`     // 终端型号，2011版本8位，2013版本20位
	DeviceID       string     `json:"deviceId"`       // 终端ID，大写字母和数字
	PlateColor     byte       `json:"plateColor"`     // 车牌颜色，JTT415-2006定义，未上牌填0
	PlateNumber    string     `json:"plateNumber"`    // 车牌号
}

func (m *Msg0100) Decode(packet *PacketData) error {
	m.Header = packet.Header
	pkt, idx := packet.Body, 0
	m.ProvinceID = hex.ReadWord(pkt, &idx)
	m.CityID = hex.ReadWord(pkt, &idx)

	ver := &m.Header.Attr.VersionDesc
	var manuLen, modeLen, idLen int
	if *ver == Version2019 {
		manuLen, modeLen, idLen = 11, 30, 30
	} else if *ver == Version2013 {
		manuLen, idLen = 5, 7
		remainLen := int(m.Header.Attr.BodyLength) - idx
		if remainLen > 5+20+7+1 { // 厂商+型号+ID+车牌颜色，2013版本至少33位
			modeLen = 20
		} else {
			modeLen = 8
			ver = &[]VersionType{Version2011}[0]
		}
	} else {
		return ErrDecodeMsg
	}
	cutset := "\x00"
	m.ManufacturerID = strings.TrimRight(hex.ReadString(pkt, &idx, manuLen), cutset)
	m.DeviceMode = strings.TrimRight(hex.ReadString(pkt, &idx, modeLen), cutset)
	m.DeviceID = strings.TrimRight(hex.ReadString(pkt, &idx, idLen), cutset)

	m.PlateColor = hex.ReadByte(pkt, &idx)
	m.PlateNumber = hex.ReadGBK(pkt, &idx, int(m.Header.Attr.BodyLength)-idx)

	return nil
}

func (m *Msg0100) Encode() (pkt []byte, err error) {
	pkt = hex.WriteWord(pkt, m.ProvinceID)
	pkt = hex.WriteWord(pkt, m.CityID)

	msgVer := m.Header.Attr.VersionDesc
	var manuLen, modeLen, idLen int // 设备厂商、型号、id长度
	if msgVer == Version2019 {
		manuLen, modeLen, idLen = 11, 30, 30
	} else if msgVer == Version2013 {
		manuLen, modeLen, idLen = 5, 20, 7
	} else if msgVer == Version2011 {
		manuLen, modeLen, idLen = 5, 8, 7
	} else {
		return nil, ErrEncodeHeader
	}
	var fillByte byte // '\x00'
	manu := []byte(m.ManufacturerID)
	toFillLen := manuLen - len(manu)
	for i := 0; i < toFillLen; i++ {
		manu = append(manu, fillByte)
	}
	pkt = append(pkt, manu...)

	mode := []byte(m.DeviceMode)
	toFillLen = modeLen - len(mode)
	for i := 0; i < toFillLen; i++ {
		mode = append(mode, fillByte)
	}
	pkt = append(pkt, mode...)

	id := []byte(m.DeviceID)
	toFillLen = idLen - len(id)
	for i := 0; i < toFillLen; i++ {
		id = append(id, fillByte)
	}
	pkt = append(pkt, id...)

	pkt = hex.WriteByte(pkt, m.PlateColor)
	pkt = hex.WriteGBK(pkt, m.PlateNumber)

	pkt, err = writeHeader(m, pkt)
	return pkt, err
}

func (m *Msg0100) GetHeader() *MsgHeader {
	return m.Header
}

func (m *Msg0100) GenOutgoing(incoming JT808Msg) error {
	return nil
}
