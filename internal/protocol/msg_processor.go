package protocol

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/fakeYanss/jt808-server-go/internal/codec/hash"
	"github.com/fakeYanss/jt808-server-go/internal/protocol/model"
	"github.com/fakeYanss/jt808-server-go/internal/storage"
)

var (
	ErrMsgIDNotSupportted = errors.New("Msg id is not supportted") // 消息ID无法处理，应忽略
	ErrNotAuthorized      = errors.New("Not authorized")           // server校验鉴权不通过
	ErrActiveClose        = errors.New("Active close")             // client无法继续处理，应主动关闭连接
)

// 处理消息的Handler接口
type MsgProcessor interface {
	Process(ctx context.Context, pkt *model.PacketData) (*model.ProcessData, error)
}

// 消息处理方法调用表, <msgId, action>
type processOptions map[uint16]*action

type action struct {
	genData func() *model.ProcessData                       // 定义生成消息的类型。由于go不支持type作为参数，所以这里直接初始化结构体
	process func(context.Context, *model.ProcessData) error // 处理消息的逻辑。可以设置消息字段、根据消息做相应处理逻辑
}

// 表驱动，初始化消息处理方法组
func initProcessOption() processOptions {
	options := make(processOptions)
	options[0x0001] = &action{ // 通用应答
		genData: func() *model.ProcessData {
			return &model.ProcessData{Incoming: &model.Msg0001{}} // 无需回复
		},
	}
	options[0x0002] = &action{ // 心跳
		genData: func() *model.ProcessData {
			return &model.ProcessData{Incoming: &model.Msg0002{}, Outgoing: &model.Msg8001{}}
		},
		process: processMsg0002,
	}
	options[0x0003] = &action{ // 注销
		genData: func() *model.ProcessData {
			return &model.ProcessData{Incoming: &model.Msg0003{}, Outgoing: &model.Msg8001{}}
		},
		process: processMsg0003,
	}
	options[0x0100] = &action{ // 注册
		genData: func() *model.ProcessData {
			return &model.ProcessData{Incoming: &model.Msg0100{}, Outgoing: &model.Msg8100{}}
		},
		process: processMsg0100,
	}
	options[0x0102] = &action{ // 鉴权
		genData: func() *model.ProcessData {
			return &model.ProcessData{Incoming: &model.Msg0102{}, Outgoing: &model.Msg8001{}}
		},
		process: processMsg0102,
	}
	options[0x0200] = &action{ // 位置信息上报
		genData: func() *model.ProcessData {
			return &model.ProcessData{Incoming: &model.Msg0200{}, Outgoing: &model.Msg8001{}}
		},
		process: handleMsg0200,
	}
	options[0x8001] = &action{ // 通用应答
		genData: func() *model.ProcessData {
			return &model.ProcessData{Incoming: &model.Msg8001{}}
		},
	}
	options[0x8100] = &action{ // 注册应答
		genData: func() *model.ProcessData {
			return &model.ProcessData{Incoming: &model.Msg8100{}, Outgoing: &model.Msg0102{}}
		},
		process: processMsg8100,
	}

	return options
}

// 处理jt808消息的Handler方法
type JT808MsgProcessor struct {
	options processOptions
}

// processor单例
var jt808MsgProcessorSingleton *JT808MsgProcessor
var processorInitOnce sync.Once

func NewJT808MsgProcessor() *JT808MsgProcessor {
	processorInitOnce.Do(func() {
		jt808MsgProcessorSingleton = &JT808MsgProcessor{
			options: initProcessOption(),
		}
	})
	return jt808MsgProcessorSingleton
}

func (mp *JT808MsgProcessor) Process(ctx context.Context, pkt *model.PacketData) (*model.ProcessData, error) {
	msgID := pkt.Header.MsgID
	genDataFn := mp.options[msgID].genData
	if genDataFn == nil {
		return nil, ErrMsgIDNotSupportted
	}
	data := genDataFn()

	in := data.Incoming
	err := in.Decode(pkt)
	if err != nil {
		return nil, errors.Wrap(err, "Fail to decode packet to jtmsg")
	}

	if log.Logger.GetLevel() == zerolog.DebugLevel {
		// print log of msg content
		session := ctx.Value(model.SessionCtxKey{}).(*model.Session)
		inJSON, err := json.Marshal(in)
		if err != nil {
			return nil, errors.Wrap(err, "Fail to serialize incoming msg to json")
		}
		log.Debug().
			Str("id", session.ID).
			RawJSON("incoming", inJSON). // for debug
			Msg("Received jt808 msg.")
	}

	if data.Outgoing == nil {
		return nil, nil // 此类型msg不需要回复
	}
	out := data.Outgoing
	err = out.GenOutgoing(in)
	if err != nil {
		return data, errors.Wrap(err, "Fail to generate outgoing msg")
	}

	// print log of outgoing content
	defer func() {
		if out == nil || log.Logger.GetLevel() != zerolog.DebugLevel {
			return
		}

		outJSON, _ := json.Marshal(out)
		session := ctx.Value(model.SessionCtxKey{}).(*model.Session)
		log.Debug().
			Str("id", session.ID).
			RawJSON("outgoing", outJSON). // for debug
			Msg("Generating jt808 outgoing msg.")
	}()

	processFunc := mp.options[msgID].process
	err = processFunc(ctx, data)
	if err != nil {
		return data, errors.Wrap(err, "Fail to process data")
	}
	return data, nil
}

// 收到心跳，应刷新终端缓存有效期
func processMsg0002(ctx context.Context, data *model.ProcessData) error {
	cache := storage.GetDeviceCache()
	device, err := cache.GetDeviceByPhone(data.Incoming.GetHeader().PhoneNumber)

	// 缓存不存在，说明设备不合法，需要返回错误，让服务层处理关闭
	if errors.Is(err, storage.ErrDeviceNotFound) {
		return errors.Wrapf(err, "Fail to find device cache, phoneNumber=%s", data.Incoming.GetHeader().PhoneNumber)
	}

	cache.CacheDevice(device)

	return nil
}

// 收到注销，应清除缓存，断开连接。
func processMsg0003(ctx context.Context, data *model.ProcessData) error {
	cache := storage.GetDeviceCache()
	device, err := cache.GetDeviceByPhone(data.Incoming.GetHeader().PhoneNumber)
	// 缓存不存在，说明设备不合法，需要返回错误，让服务层处理关闭
	if errors.Is(err, storage.ErrDeviceNotFound) {
		return errors.Wrapf(err, "Fail to find device cache, phoneNumber=%s", data.Incoming.GetHeader().PhoneNumber)
	}
	// 取消定时任务
	timer := NewKeepaliveTimer()
	timer.Cancel(device.PhoneNumber)
	// 清楚缓存
	cache.DelDeviceByPhone(device.PhoneNumber)
	// 为避免连接TIMEWAIT，应等待对方主动关闭
	return nil
}

// 收到注册，应校验设备ID，如果可注册，则缓存设备信息并返回鉴权码
func processMsg0100(ctx context.Context, data *model.ProcessData) error {
	in := data.Incoming.(*model.Msg0100)

	cache := storage.GetDeviceCache()
	// 校验注册逻辑
	out := data.Outgoing.(*model.Msg8100)
	// 车辆已被注册
	if cache.HasPlate(in.PlateNumber) {
		out.Result = model.ResCarAlreadyRegister
		return nil
	}
	// 终端已被注册
	if cache.HasPhone(in.Header.PhoneNumber) {
		out.Result = model.ResDeviceAlreadyRegister
		return nil
	}

	session := ctx.Value(model.SessionCtxKey{}).(*model.Session)
	device := &model.Device{
		ID:          in.DeviceID,
		PlateNumber: in.PlateNumber,
		PhoneNumber: in.Header.PhoneNumber,
		SessionID:   session.ID,
		TransProto:  session.GetTransProto(),
		Conn:        session.Conn,
		Keepalive:   time.Minute * 1,
		Status:      model.DeviceStatusOffline,
	}
	out.AuthCode = genAuthCode(device) // 设置鉴权码
	cache.CacheDevice(device)

	timer := NewKeepaliveTimer()
	timer.Register(device.PhoneNumber)
	return nil
}

// 收到鉴权，应校验鉴权token
func processMsg0102(ctx context.Context, data *model.ProcessData) error {
	in := data.Incoming.(*model.Msg0102)

	cache := storage.GetDeviceCache()
	device, err := cache.GetDeviceByPhone(in.Header.PhoneNumber)
	// 缓存不存在，说明设备不合法，需要返回错误，让服务层处理关闭
	if errors.Is(err, storage.ErrDeviceNotFound) {
		return errors.Wrapf(err, "Fail to find device cache, phoneNumber=%s", in.Header.PhoneNumber)
	}

	out := data.Outgoing.(*model.Msg8001)
	// 校验鉴权逻辑
	if in.AuthCode != genAuthCode(device) {
		out.Result = model.ResultFail
		// 取消定时任务
		timer := NewKeepaliveTimer()
		timer.Cancel(device.PhoneNumber)
		// 删除设备缓存
		cache.DelDeviceByPhone(device.PhoneNumber)
	} else {
		// 鉴权通过
		device.Status = model.DeviceStatusOnline
		cache.CacheDevice(device)
	}

	return nil
}

func genAuthCode(d *model.Device) string {
	var splitByte byte = '_'
	codeBuilder := new(strings.Builder)
	codeBuilder.WriteString(string(d.ID))
	codeBuilder.WriteByte(splitByte)
	codeBuilder.Write([]byte(d.PlateNumber))
	codeBuilder.WriteByte(splitByte)
	codeBuilder.Write([]byte(d.PhoneNumber))
	return strconv.Itoa(int(hash.FNV32(codeBuilder.String())))
}

// 收到位置信息汇报，回复通用应答
func handleMsg0200(ctx context.Context, data *model.ProcessData) error {
	in := data.Incoming.(*model.Msg0200)

	cache := storage.GetDeviceCache()
	device, err := cache.GetDeviceByPhone(in.Header.PhoneNumber)
	// 缓存不存在，说明设备不合法，需要返回错误，让服务层处理关闭
	if errors.Is(err, storage.ErrDeviceNotFound) {
		return errors.Wrapf(err, "Fail to find device cache, phoneNumber=%s", in.Header.PhoneNumber)
	}

	// 解析状态位编码
	gis := model.NewGISMeta()
	gis.Decode(in.StatusSign)

	if gis.ACCStatus == 0 { // ACC关闭，设备休眠
		device.Status = model.DeviceStatusSleeping
		cache.CacheDevice(device)
	}

	gisCache := storage.GetGisCache()
	rb := gisCache.GetGisRingByPhone(device.ID)
	rb.Write(gis)

	return nil
}

// 收到注册应答，回复鉴权
func processMsg8100(ctx context.Context, data *model.ProcessData) error {
	in := data.Incoming.(*model.Msg8100)
	out := data.Outgoing.(*model.Msg0102)

	cache := storage.GetDeviceCache()
	device, err := cache.GetDeviceByPhone(in.Header.PhoneNumber)
	if errors.Is(err, storage.ErrDeviceNotFound) {
		return ErrActiveClose
	}

	out.AuthCode = genAuthCode(device)
	out.IMEI = device.IMEI
	out.SoftwareVersion = device.SoftwareVersion
	err = out.GenOutgoing(in)
	if err != nil {
		return errors.Wrap(err, "Fail to generate msg 8100")
	}

	return nil
}
