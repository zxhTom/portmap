package protocol

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"
)

type CtrlMsg struct {
	Type       string `json:"type"`
	MappingID  string `json:"mapping_id,omitempty"`
	TargetHost string `json:"target_host,omitempty"`
	TargetPort int    `json:"target_port,omitempty"`
	LocalPort  int    `json:"local_port,omitempty"`
	StreamID   uint32 `json:"stream_id,omitempty"`
	Message    string `json:"message,omitempty"`
	User       string `json:"user,omitempty"`
	Password   string `json:"password,omitempty"`
	Token      string `json:"token,omitempty"`
}

type MuxConn struct {
	conn   net.Conn
	mu     sync.Mutex
	rbuf   [8]byte
	wbuf   [8]byte
	ctrlCh chan CtrlMsg
}

func NewMuxConn(conn net.Conn) *MuxConn {
	return &MuxConn{
		conn:   conn,
		ctrlCh: make(chan CtrlMsg, 64),
	}
}

func (m *MuxConn) Conn() net.Conn { return m.conn }

func (m *MuxConn) WriteFrame(streamID uint32, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	binary.BigEndian.PutUint32(m.wbuf[0:4], streamID)
	binary.BigEndian.PutUint32(m.wbuf[4:8], uint32(len(data)))

	if _, err := m.conn.Write(m.wbuf[:]); err != nil {
		return err
	}
	if len(data) > 0 {
		if _, err := m.conn.Write(data); err != nil {
			return err
		}
	}
	return nil
}

func (m *MuxConn) ReadFrame() (streamID uint32, data []byte, err error) {
	if _, err := io.ReadFull(m.conn, m.rbuf[:]); err != nil {
		return 0, nil, err
	}
	streamID = binary.BigEndian.Uint32(m.rbuf[0:4])
	length := binary.BigEndian.Uint32(m.rbuf[4:8])

	if length > 0 {
		data = make([]byte, length)
		if _, err := io.ReadFull(m.conn, data); err != nil {
			return 0, nil, err
		}
	}
	return streamID, data, nil
}

// StartReadLoop reads frames and dispatches: stream 0 → ctrlCh, streams > 0 → handler.
func (m *MuxConn) StartReadLoop(handler func(streamID uint32, data []byte)) {
	go func() {
		for {
			streamID, data, err := m.ReadFrame()
			if err != nil {
				m.ctrlCh <- CtrlMsg{Type: "closed"}
				return
			}
			if streamID == 0 {
				var msg CtrlMsg
				if err := json.Unmarshal(data, &msg); err != nil {
					continue
				}
				m.ctrlCh <- msg
			} else {
				handler(streamID, data)
			}
		}
	}()
}

func (m *MuxConn) SendCtrl(msg CtrlMsg) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return m.WriteFrame(0, data)
}

func (m *MuxConn) CtrlChan() <-chan CtrlMsg {
	return m.ctrlCh
}

// CopyStream copies bidirectionally between a net.Conn and a mux stream.
func CopyStream(mux *MuxConn, streamID uint32, conn net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				if werr := mux.WriteFrame(streamID, buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				mux.SendCtrl(CtrlMsg{Type: "stream_close", StreamID: streamID})
				break
			}
		}
		done <- struct{}{}
	}()
	<-done
}

func ErrorMsg(err error) CtrlMsg {
	return CtrlMsg{Type: "error", Message: fmt.Sprintf("%v", err)}
}
