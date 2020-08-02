package dht

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/marksamman/bencode"
)

const (
	perBlock        = 16384
	maxMetadataSize = perBlock * 1024
	extended        = 20
	extHandshake    = 0
)

type Meta struct {
	addr         string
	infoHash     []byte
	infoHashHex  string
	timeout      time.Duration
	conn         net.Conn
	peerId       string
	preHeader    []byte
	metadataSize int64
	utMetadata   int64
	pieceCount   int64
	pieces       [][]byte
}

func NewMeta(peerId, addr string, hash []byte) *Meta {
	return &Meta{
		addr:        addr,
		infoHash:    hash,
		infoHashHex: hex.EncodeToString(hash),
		timeout:     3 * time.Second,
		peerId:      peerId,
		preHeader:   MakePreHeader(),
	}
}

func (mw *Meta) checkDone() bool {
	for _, b := range mw.pieces {
		if b == nil {
			return false
		}
	}
	return true
}

func (m *Meta) readOnePiece(payload []byte) error {
	trailerIndex := bytes.Index(payload, []byte("ee")) + 2
	if trailerIndex == 1 {
		return errors.New("ee == 1")
	}

	dict, err := bencode.Decode(bytes.NewBuffer(payload[:trailerIndex]))
	if err != nil {
		return err
	}

	pieceIndex, ok := dict["piece"].(int64)
	if !ok || pieceIndex >= m.pieceCount {
		return errors.New("piece num error")
	}

	msgType, ok := dict["msg_type"].(int64)
	if !ok || msgType != 1 {
		return errors.New("piece type error")
	}
	m.pieces[pieceIndex] = payload[trailerIndex:]
	return nil
}

func (m *Meta) Begin() ([]byte, error) {
	m.SetDeadLine(30)

	for i := 0; i < int(m.pieceCount); i++ {
		m.requestPiece(i)
	}

	for {
		data, err := m.ReadN()
		if err != nil {
			return nil, err
		}
		fmt.Println("read data:", data)
		if data[0] != extended {
			continue
		}

		if data[1] != 1 {
			continue
		}
		err = m.readOnePiece(data[2:])
		if err != nil {
			return nil, err
		}

		if !m.checkDone() {
			continue
		}

		pie := bytes.Join(m.pieces, []byte(""))
		sum := sha1.Sum(pie)
		if bytes.Equal(sum[:], m.infoHash) {
			return pie, nil
		}

		return nil, errors.New("metadata checksum mismatch")
	}
}

func (m *Meta) Start() {
	err := m.Connect()
	defer m.conn.Close()
	if err != nil {
		fmt.Printf("connect err:%s\n", err.Error())
		return
	}
	fmt.Println("connect finish")
	ret, err := m.Begin()
	if err != nil {
		fmt.Printf("read  err:%s\n", err.Error())
		return
	}
	fmt.Println("begin finish")
	dict, err := bencode.Decode(bytes.NewBuffer(ret))
	if err != nil {
		fmt.Printf("dec ret ode  err:%s\n", err.Error())
		return
	}
	fmt.Printf("%v\n", dict)
}

func (m *Meta) SetDeadLine(second time.Duration) {
	deadLine := time.Now().Add(second * time.Second)
	m.conn.SetReadDeadline(deadLine)
	m.conn.SetWriteDeadline(deadLine)
}

func (m *Meta) Connect() error {
	var err error
	m.conn, err = net.DialTimeout("tcp", m.addr, m.timeout)
	if err != nil {
		fmt.Printf("dail tcp err:%s\n", err.Error())
		return err
	}
	m.SetDeadLine(5)
	err = m.HandShake()
	if err != nil {
		return err
	}
	return m.extHandShake()
}

func (m *Meta) WriteTo(data []byte) error {

	length := uint32(len(data))

	lenByte := make([]byte, 4)
	binary.BigEndian.PutUint32(lenByte, length)

	//buf := bytes.NewBuffer(nil)
	//binary.Write(buf, binary.BigEndian, length)
	//buf.Write(data)
	_, err := m.conn.Write(append(lenByte, data...))
	if err != nil {
		return fmt.Errorf("write message failed: %v", err)
	}

	return nil
}

func (m *Meta) ReadN() ([]byte, error) {
	length := make([]byte, 4)
	_, err := io.ReadFull(m.conn, length)
	if err != nil {
		return nil, err
	}

	size := binary.BigEndian.Uint32(length)

	data := make([]byte, size)
	_, err = io.ReadFull(m.conn, data)
	if err != nil {
		return nil, err
	}

	return data, nil
}

func (m *Meta) extHandShake() error {
	//etxHandShark
	data := append([]byte{extended, extHandshake}, bencode.Encode(map[string]interface{}{
		"m": map[string]interface{}{
			"ut_metadata": 1,
		},
	})...)
	if err := m.WriteTo(bencode.Encode(data)); err != nil {
		return err
	}

	data, err := m.ReadN()
	if err != nil {
		return err
	}

	if data[0] != extended {
		return errors.New("data 0 err")
	}
	if data[1] != 0 {
		return errors.New("data 1 err")
	}
	return m.onExtHandshake(data[2:])
}

func (m *Meta) HandShake() error {
	buf := bytes.NewBuffer(nil)
	buf.Write(m.preHeader)
	buf.Write(m.infoHash)
	buf.WriteString(m.peerId)
	_, err := m.conn.Write(buf.Bytes())

	res := make([]byte, 68)
	n, err := io.ReadFull(m.conn, res)
	if err != nil {
		return err
	}
	if n != 68 {
		return errors.New("hand read len err")
	}

	if !bytes.Equal(res[:20], m.preHeader[:20]) {
		return errors.New("remote peer not supporting bittorrent protocol")
	}

	if res[25]&0x10 != 0x10 {
		return errors.New("remote peer not supporting extension protocol")
	}

	if !bytes.Equal(res[28:48], m.infoHash) {
		return errors.New("invalid bittorrent header response")
	}
	return nil
}

func (this *Meta) onExtHandshake(payload []byte) error {

	dict, err := bencode.Decode(bytes.NewBuffer(payload))
	if err != nil {
		return err
	}

	metadataSize, ok := dict["metadata_size"].(int64)
	if !ok {
		return errors.New("invalid extension header response")
	}

	if metadataSize > maxMetadataSize {
		return errors.New("metadata_size too long")
	}

	if metadataSize < 0 {
		return errors.New("negative metadata_size")
	}

	m, ok := dict["m"].(map[string]interface{})
	if !ok {
		return errors.New("negative metadata m")
	}

	utMetadata, ok := m["ut_metadata"].(int64)
	if !ok {
		return errors.New("negative metadata ut_metadata")
	}

	this.metadataSize = metadataSize
	this.utMetadata = utMetadata
	this.pieceCount = metadataSize / perBlock
	if this.metadataSize%perBlock != 0 {
		this.pieceCount++
	}
	this.pieces = make([][]byte, this.pieceCount)

	return nil
}

func (mw *Meta) requestPiece(i int) {
	buf := bytes.NewBuffer(nil)
	buf.WriteByte(byte(extended))
	buf.WriteByte(byte(mw.utMetadata))
	buf.Write(bencode.Encode(map[string]interface{}{
		"msg_type": 0,
		"piece":    i,
	}))
	mw.WriteTo(buf.Bytes())
}
