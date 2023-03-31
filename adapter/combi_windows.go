package adapter

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"runtime"
	"time"

	"github.com/google/gousb"
	"github.com/roffe/gocan"
	"github.com/smallnest/ringbuffer"
)

const (
	cmdtxFrame       = 0x83
	cmdrxFrame       = 0x82
	cmdSetCanBitrate = 0x81
	cmdOpen          = 0x80
	cmdVersion       = 0x20
)

type CombiPacket struct {
	cmd  uint8
	len  uint16
	data []byte
	term uint8
}

func (cp *CombiPacket) Bytes() []byte {
	buf := new(bytes.Buffer)
	buf.WriteByte(cp.cmd)
	buf.Write([]byte{uint8(cp.len >> 8), uint8(cp.len & 0xff)})
	if cp.data != nil {
		buf.Write(cp.data)
	}
	buf.WriteByte(cp.term)
	return buf.Bytes()
}

type CombiAdapter struct {
	cfg        *gocan.AdapterConfig
	send, recv chan gocan.CANFrame
	close      chan struct{}
	usbCtx     *gousb.Context
	dev        *gousb.Device
	devCfg     *gousb.Config
	iface      *gousb.Interface
	in         *gousb.InEndpoint
	out        *gousb.OutEndpoint
	sendSem    chan struct{}
}

func init() {
	if !findCombi() {
		return
	}
	if err := Register(&AdapterInfo{
		Name:               "CombiAdapter",
		Description:        "libusb driver",
		RequiresSerialPort: false,
		Capabilities: AdapterCapabilities{
			HSCAN: true,
			KLine: false,
			SWCAN: false,
		},
		New: NewCombi,
	}); err != nil {
		panic(err)
	}
}

func findCombi() bool {
	ctx := gousb.NewContext()
	defer ctx.Close()
	dev, err := ctx.OpenDeviceWithVIDPID(0xFFFF, 0x0005)
	if err != nil || dev == nil {
		return false
	}
	defer dev.Close()
	return true
}

func NewCombi(cfg *gocan.AdapterConfig) (gocan.Adapter, error) {
	return &CombiAdapter{
		cfg:     cfg,
		send:    make(chan gocan.CANFrame, 10),
		recv:    make(chan gocan.CANFrame, 20),
		close:   make(chan struct{}, 1),
		sendSem: make(chan struct{}, 1),
	}, nil
}

func (ca *CombiAdapter) SetFilter(filters []uint32) error {
	return nil
}

func (ca *CombiAdapter) Name() string {
	return "CombiAdapter"
}

func (ca *CombiAdapter) Init(ctx context.Context) error {
	ca.usbCtx = gousb.NewContext()

	var err error

	ca.dev, err = ca.usbCtx.OpenDeviceWithVIDPID(0xFFFF, 0x0005)
	if err != nil && ca.dev == nil {
		if err := ca.usbCtx.Close(); err != nil {
			ca.cfg.OnError(fmt.Errorf("failed to close usb context: %w", err))
		}
		return err
	} else if err != nil && ca.dev != nil {
		if err := ca.dev.Close(); err != nil {
			ca.cfg.OnError(fmt.Errorf("failed to close usb device: %w", err))
		}
		if err := ca.usbCtx.Close(); err != nil {
			ca.cfg.OnError(fmt.Errorf("failed to close usb context: %w", err))
		}
		return err
	}

	if err := ca.dev.SetAutoDetach(true); err != nil {
		ca.cfg.OnError(fmt.Errorf("failed to set auto detach: %w", err))
	}

	ca.devCfg, err = ca.dev.Config(1)
	if err != nil {
		if err := ca.dev.Close(); err != nil {
			ca.cfg.OnError(fmt.Errorf("failed to close usb device: %w", err))
		}
		if err := ca.usbCtx.Close(); err != nil {
			ca.cfg.OnError(fmt.Errorf("failed to close usb context: %w", err))
		}
		return err
	}

	ca.iface, err = ca.devCfg.Interface(1, 0)
	if err != nil {
		if err := ca.devCfg.Close(); err != nil {
			ca.cfg.OnError(fmt.Errorf("failed to close usb device config: %w", err))
		}
		if err := ca.dev.Close(); err != nil {
			ca.cfg.OnError(fmt.Errorf("failed to close usb device: %w", err))
		}
		if err := ca.usbCtx.Close(); err != nil {
			ca.cfg.OnError(fmt.Errorf("failed to close usb context: %w", err))
		}
		return err
	}

	ca.in, err = ca.iface.InEndpoint(2)
	if err != nil {
		ca.closeAdapter(false)
	}

	ca.out, err = ca.iface.OutEndpoint(5)
	if err != nil {
		ca.closeAdapter(false)
	}

	if _, err := ca.out.Write((&CombiPacket{cmd: cmdOpen, len: 1, data: []byte{0}, term: 0x00}).Bytes()); err != nil {
		ca.closeAdapter(false)
		ca.cfg.OnError(fmt.Errorf("failed to write to usb device: %w", err))
		return err
	}

	if ca.cfg.PrintVersion {
		if ver, err := ca.ReadVersion(ctx); err == nil {
			ca.cfg.OnMessage(ver)
		}
	}

	if err := ca.setBitrate(ctx); err != nil {
		ca.closeAdapter(false)
		return err
	}

	if _, err := ca.out.Write((&CombiPacket{cmd: cmdOpen, len: 1, data: []byte{1}, term: 0x00}).Bytes()); err != nil {
		ca.closeAdapter(false)
		ca.cfg.OnError(fmt.Errorf("failed to write to usb device: %w", err))
		return err
	}

	go ca.recvManager()
	go ca.sendManager()

	return nil
}

func (ca *CombiAdapter) Close() error {
	return ca.closeAdapter(true)
}

func (ca *CombiAdapter) closeAdapter(sendClose bool) error {
	if sendClose {
		if _, err := ca.out.Write((&CombiPacket{cmd: cmdOpen, len: 1, data: []byte{0}, term: 0x00}).Bytes()); err != nil {
			ca.cfg.OnError(fmt.Errorf("failed to close device: %w", err))
		}
		time.Sleep(200 * time.Millisecond)
	}
	close(ca.close)

	time.Sleep(10 * time.Millisecond)

	ca.iface.Close()
	if err := ca.devCfg.Close(); err != nil {
		ca.cfg.OnError(fmt.Errorf("failed to close device config: %w", err))
	}
	if err := ca.dev.Close(); err != nil {
		ca.cfg.OnError(fmt.Errorf("failed to close device: %w", err))
	}
	if err := ca.usbCtx.Close(); err != nil {
		ca.cfg.OnError(fmt.Errorf("failed to close usb context: %w", err))
	}
	return nil
}

func (ca *CombiAdapter) sendManager() {
	runtime.LockOSThread()
	sw, err := ca.out.NewStream(ca.out.Desc.MaxPacketSize, 1)
	if err != nil {
		ca.cfg.OnError(fmt.Errorf("failed to create stream writer: %w", err))
	}
	for {
		select {
		case <-ca.close:
			return
		case f := <-ca.send:
			ca.sendSem <- struct{}{}
			if _, err := sw.Write(ca.frameToTxBytes(f)); err != nil {
				ca.cfg.OnError(fmt.Errorf("failed to send frame: %w", err))
			}
		}
	}
}

func (ca *CombiAdapter) frameToTxBytes(frame gocan.CANFrame) []byte {
	buff := make([]byte, 19)
	buff[0] = cmdtxFrame
	buff[1] = 15 >> 8
	buff[2] = 15 & 0xff
	binary.LittleEndian.PutUint32(buff[3:], frame.Identifier())
	copy(buff[7:], frame.Data())
	buff[15] = uint8(frame.Length())
	buff[16] = 0x00 // is extended
	buff[17] = 0x00 // is remote
	buff[18] = 0x00 // terminator
	return buff
}

/*
func frameToPacket(frame gocan.CANFrame) *CombiPacket {
	buff := make([]byte, 15)
	binary.LittleEndian.PutUint32(buff, frame.Identifier())
	copy(buff[4:], frame.Data())
	buff[12] = uint8(frame.Length())
	buff[13] = 0
	buff[14] = 0
	return &CombiPacket{
		cmd:  cmdtxFrame,
		len:  15,
		data: buff,
		term: 0x00,
	}
}
*/
/*
func (ca *CombiAdapter) sendFrame(ctx context.Context, frame gocan.CANFrame) error {
	buff := make([]byte, 15)
	binary.LittleEndian.PutUint32(buff, frame.Identifier())
	copy(buff[4:], frame.Data())
	buff[12] = uint8(frame.Length())
	buff[13] = 0
	buff[14] = 0
	tx := &CombiPacket{
		cmd:  cmdtxFrame,
		len:  15,
		data: buff,
		term: 0x00,
	}
	b := tx.Bytes()
	ca.sendSem <- struct{}{}
	n, err := ca.out.Write(b)
	if n != len(b) {
		ca.cfg.OnError(fmt.Errorf("sent %d bytes of data out of %d", n, len(b)))
	}
	if err != nil {
		return err
	}
	return nil
}
*/

func (ca *CombiAdapter) recvManager() {
	f, err := os.Create("recv.log")
	if err != nil {
		ca.cfg.OnError(fmt.Errorf("failed to create log file: %w", err))
		return
	}

	rb := ringbuffer.New(ca.in.Desc.MaxPacketSize * 10)
	buff := make([]byte, ca.in.Desc.MaxPacketSize)
	rs, err := ca.in.NewStream(ca.in.Desc.MaxPacketSize, 8)
	if err != nil {
		ca.cfg.OnError(fmt.Errorf("failed to create read stream: %w", err))
		return
	}
	go func() {
		for {
			select {
			case <-ca.close:
				return
			default:
				n, err := rs.Read(buff)
				if err != nil {
					ca.cfg.OnError(fmt.Errorf("failed to read from usb: %w", err))
					continue
				}
				f.WriteString(fmt.Sprintf("%X", buff[:n]) + "\n")
				if _, err := rb.Write(buff[:n]); err != nil {
					ca.cfg.OnError(fmt.Errorf("failed to write to ringbuffer: %w", err))
					continue
				}
			}
		}
	}()

	for {
		select {
		case <-ca.close:
			return
		default:
			if rb.IsEmpty() {
				continue
			}
			cmd, err := rb.ReadByte()
			if err != nil {
				ca.cfg.OnError(fmt.Errorf("failed to read cmd from ringbuffer: %w", err))
				continue
			}

			switch cmd {
			case cmdrxFrame:
				for rb.Length() < 2 {
					//log.Println("waiting for rx Data")
					time.Sleep(ca.in.Desc.PollInterval)
				}
			case cmdtxFrame:
				select {
				case <-ca.sendSem:
				default:
				}
				for rb.Length() < 3 {
					log.Println("waiting for tx Data")
					time.Sleep(ca.in.Desc.PollInterval)
				}
			default:
				for rb.Length() < 3 {
					log.Printf("waiting for Data for cmd %X", cmd)
					time.Sleep(ca.in.Desc.PollInterval)
				}
			}

			lenBytes := make([]byte, 2)
			if _, err := rb.Read(lenBytes); err != nil {
				ca.cfg.OnError(fmt.Errorf("failed to read len from ringbuffer: %w", err))
			}
			dataLen := int(lenBytes[0])<<8 | int(lenBytes[1])

			if cmd == cmdrxFrame {
				for rb.Length() < dataLen+1 {
					//log.Println("waiting for rx2 Data")
					time.Sleep(ca.in.Desc.PollInterval)
				}
			}

			//var data []byte
			data := make([]byte, dataLen)
			if dataLen > 0 {
				n, err := rb.Read(data)
				if err != nil {
					ca.cfg.OnError(fmt.Errorf("failed to read data from ringbuffer: %w", err))
				}
				if n != dataLen {
					ca.cfg.OnError(fmt.Errorf("read %d bytes, expected %d", n, dataLen))
				}
			}

			term, err := rb.ReadByte()
			if err != nil {
				ca.cfg.OnError(fmt.Errorf("failed to read term from ringbuffer: %w", err))
			}

			switch cmd {

			case cmdtxFrame, cmdVersion, cmdOpen:
			case cmdrxFrame: //rx
				frame := gocan.NewFrame(
					binary.LittleEndian.Uint32(data[:4]),
					data[4:4+data[12]],
					gocan.Incoming,
				)
				ca.recv <- frame
			default:
				//log.Printf("cmd: %02X, len: %d, data: %X, term: %02X", cmd, dataLen, data, term)
				f.WriteString(fmt.Sprintf("cmd: %02X, len: %d, data: %X, term: %02X", cmd, dataLen, data, term) + "\n")
			}
		}
	}
}

func (ca *CombiAdapter) setBitrate(ctx context.Context) error {
	canrate := make([]byte, 4)
	binary.BigEndian.PutUint32(canrate, uint32(ca.cfg.CANRate*1000))
	tx := CombiPacket{
		cmd:  cmdSetCanBitrate,
		len:  4,
		data: canrate,
		term: 0,
	}
	if _, err := ca.out.Write(tx.Bytes()); err != nil {
		ca.cfg.OnError(fmt.Errorf("failed to set bitrate: %w", err))
		return err
	}

	return nil
}

func (ca *CombiAdapter) Recv() <-chan gocan.CANFrame {
	return ca.recv
}

func (ca *CombiAdapter) Send() chan<- gocan.CANFrame {
	return ca.send
}

func (ca *CombiAdapter) ReadVersion(ctx context.Context) (string, error) {
	tx := CombiPacket{
		cmd:  cmdVersion,
		len:  0,
		data: nil,
		term: 0,
	}
	ca.out.Write(tx.Bytes())
	vers := make([]byte, ca.in.Desc.MaxPacketSize)
	_, err := ca.in.Read(vers)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("CombiAdapter: v%d.%d", vers[8], vers[7]), nil
}
