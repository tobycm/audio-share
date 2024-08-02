package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"os"
	"time"

	"github.com/alexflint/go-arg"
	"github.com/ebitengine/oto/v3"
	"github.com/hajimehoshi/go-mp3"
	"google.golang.org/protobuf/proto"

	"tobycm.dev/audio-share-go-client/pb"
)

var audioFormat = &pb.AudioFormat{}

var autoReconnectingTries = 0

var args struct {
	Host string `arg:"positional,required" help:"Host to connect to."`
	Port int    `arg:"positional" default:"65530" help:"Port to connect to."`

	AutoReconnect bool   `arg:"-r" help:"Automatically reconnect on connection loss."`
	OnConnect     string `arg:"-c" help:"Sound to play on connection."`
	OnDisconnect  string `arg:"-d" help:"Sound to play on disconnect."`

	Verbose    bool `arg:"-v" help:"Verbose output."`
	TcpTimeout int  `arg:"-t" default:"3000" help:"TCP timeout in seconds."`
}

var otoCtx *oto.Context

type Int16ToFloat32Converter struct {
	reader io.Reader
}

func (c *Int16ToFloat32Converter) Read(p []byte) (n int, err error) {
	// We need a temporary buffer to read int16 values since each int16 will be converted to float32,
	// and float32 is twice the size of int16.
	tempBuf := make([]byte, len(p)/2)
	n, err = c.reader.Read(tempBuf)
	if err != nil {
		return
	}

	// Initialize a buffer to store the float32 values.
	floatBuf := make([]byte, n*2)
	for i := 0; i < n; i += 2 {
		sample := int16(tempBuf[i]) | int16(tempBuf[i+1])<<8
		floatSample := float32(sample) / float32(1<<15)
		binary.LittleEndian.PutUint32(floatBuf[i*2:], math.Float32bits(floatSample))
	}

	// Copy the float32 buffer into the original buffer.
	copy(p, floatBuf)
	return n * 2, nil
}

// Command represents the command types
type Command int

const (
	CMD_NONE Command = iota
	CMD_GET_FORMAT
	CMD_START_PLAY
	CMD_HEARTBEAT
)

func (cmd Command) String() string {
	return [...]string{"CMD_NONE", "CMD_GET_FORMAT", "CMD_START_PLAY", "CMD_HEARTBEAT"}[cmd]
}

// TcpMessage represents a message sent over TCP
type TcpMessage struct {
	Command     Command
	AudioFormat *pb.AudioFormat
	ID          int
}

// Encode encodes the TcpMessage into a byte slice
func (tm *TcpMessage) Encode() []byte {
	buf := make([]byte, 4)

	// Command
	binary.LittleEndian.PutUint32(buf, uint32(tm.Command))

	return buf
}

// Decode decodes a byte slice into a TcpMessage
func (message *TcpMessage) Decode(data []byte) error {
	if len(data) < 4 {
		return fmt.Errorf("data too short to decode command")
	}

	// Command
	message.Command = Command(binary.LittleEndian.Uint32(data[:4]))
	data = data[4:]

	// AudioFormat if CMD_GET_FORMAT
	if message.Command == CMD_GET_FORMAT {
		if len(data) < 4 {
			return fmt.Errorf("data too short to decode audio format")
		}

		bufSize := int32(binary.LittleEndian.Uint32(data[:4]))
		data = data[4:]

		if len(data) < int(bufSize) {
			return fmt.Errorf("data too short to decode audio format")
		}

		message.AudioFormat = &pb.AudioFormat{}
		if err := proto.Unmarshal(data[:bufSize], message.AudioFormat); err != nil {
			return fmt.Errorf("failed to unmarshal audio format: %v", err)
		}
	}

	// ID if CMD_START_PLAY
	if message.Command == CMD_START_PLAY {
		if len(data) < 4 { // 1 int for the ID
			return fmt.Errorf("data too short to decode ID")
		}

		message.ID = int(binary.LittleEndian.Uint32(data[:4]))

		slog.Debug(fmt.Sprintf("Decoded ID: %v", message.ID))
	}

	return nil
}

func handleIncomingTCPData(conn *net.Conn) error {
	buf := make([]byte, 1024)

	for {
		length, err := (*conn).Read(buf)
		if err != nil {
			if err.Error() == "EOF" {
				slog.Info("Connection closed by server.")
				break
			}

			slog.Error(fmt.Sprintf("Error reading: %v", err))
			return err
		}

		msg := &TcpMessage{}
		if err := msg.Decode(buf[:length]); err != nil {
			slog.Error(fmt.Sprintf("Error decoding: %v", err))
			continue
		}

		if msg.Command == CMD_HEARTBEAT {
			slog.Debug("Received heartbeat")

			(*conn).Write((&TcpMessage{Command: CMD_HEARTBEAT}).Encode())

			slog.Debug("Sent heartbeat")
		} else {
			slog.Debug(fmt.Sprintf("Received message: %v", msg))
		}
	}

	return nil
}

func InitOto() error {
	op := &oto.NewContextOptions{
		SampleRate:   int(audioFormat.SampleRate),
		ChannelCount: int(audioFormat.Channels),
		Format:       oto.FormatFloat32LE,
	}

	var readyChan chan struct{}
	var err error

	otoCtx, readyChan, err = oto.NewContext(op)
	if err != nil {
		return (fmt.Errorf("failed to create new context for audio output: %v", err))
	}

	<-readyChan

	return nil
}

func LoadFileAndPlay(file string) error {
	fileBytes, err := os.ReadFile(args.OnConnect)
	if err != nil {
		return err
	}

	// Convert the pure bytes into a reader object that can be used with the mp3 decoder
	fileBytesReader := bytes.NewReader(fileBytes)

	// Decode file
	decodedMp3, err := mp3.NewDecoder(fileBytesReader)
	if err != nil {
		return err
	}

	player := otoCtx.NewPlayer(&Int16ToFloat32Converter{reader: decodedMp3})
	player.Play()

	slog.Debug("Playing on connect sound")

	for player.IsPlaying() {
		time.Sleep(time.Millisecond)
	}

	if err := player.Close(); err != nil {
		return err
	}

	return nil
}

type ConnectionState int

const (
	Connected ConnectionState = iota
	Disconnected
)

var state ConnectionState = Disconnected

func Init() error {
	tcpConn, err := net.Dial("tcp", fmt.Sprintf("%s:%d", args.Host, args.Port))
	if err != nil {
		return err
	}
	defer tcpConn.Close()

	tcpConn.SetReadDeadline(time.Now().Add(time.Duration(args.TcpTimeout) * time.Second))

	slog.Info("Connected to server.")

	// Send get format
	tcpConn.Write((&TcpMessage{Command: CMD_GET_FORMAT}).Encode())

	buf := make([]byte, 1024)

	if _, err := tcpConn.Read(buf); err != nil {
		return err
	}

	msg := &TcpMessage{}
	if err := msg.Decode(buf); err != nil {
		return err
	}

	if msg.Command != CMD_GET_FORMAT {
		return fmt.Errorf("expected CMD_GET_FORMAT")
	}

	audioFormat = msg.AudioFormat

	slog.Info(fmt.Sprintf("Audio format: %v", audioFormat))

	if otoCtx == nil {
		err = InitOto()
		if err != nil {
			return err
		}
	}

	if args.OnConnect != "" {
		if err := LoadFileAndPlay(args.OnConnect); err != nil {
			return err
		}
	}

	state = Connected

	// Send start play
	tcpConn.Write((&TcpMessage{Command: CMD_START_PLAY}).Encode())

	buf = make([]byte, 1024)
	if _, err := tcpConn.Read(buf); err != nil {
		return err
	}

	msg = &TcpMessage{}
	if err := msg.Decode(buf); err != nil {
		return err
	}

	if msg.Command != CMD_START_PLAY {
		return fmt.Errorf("expected CMD_START_PLAY")
	}

	// start UDP listener
	udpConn, err := net.Dial("udp", fmt.Sprintf("%s:%d", args.Host, args.Port))
	if err != nil {
		return err
	}
	defer udpConn.Close()

	buf = make([]byte, 4)
	binary.LittleEndian.PutUint32(buf, uint32(msg.ID))
	udpConn.Write(buf)

	player := otoCtx.NewPlayer(bufio.NewReader(udpConn))
	defer player.Close()

	player.Reset()
	player.Play()

	if err := handleIncomingTCPData(&tcpConn); err != nil {
		return err
	}

	return nil
}

func main() {
	arg.MustParse(&args)

	if args.Verbose {
		slog.SetLogLoggerLevel(slog.LevelDebug)
	}

	slog.Info(fmt.Sprintf("Host: %v", args.Host))
	slog.Info(fmt.Sprintf("Port: %v", args.Port))

	if args.AutoReconnect {
		slog.Info("Auto reconnect enabled.")
	}

	if args.OnConnect != "" {
		slog.Info(fmt.Sprintf("On connect sound: %s", args.OnConnect))
	}

	if args.OnDisconnect != "" {
		slog.Info(fmt.Sprintf("On disconnect sound: %s", args.OnDisconnect))
	}

	if err := Init(); err != nil {
		slog.Error(fmt.Sprintf("%v", err))

		if state == Connected && args.OnDisconnect != "" {
			if err := LoadFileAndPlay(args.OnDisconnect); err != nil {
				slog.Error(fmt.Sprintf("%v", err))
			}
		}
	}

	state = Disconnected

	for args.AutoReconnect {

		sleepTime := 0 * time.Second

		switch {
		case autoReconnectingTries == 0:
			sleepTime = 0 * time.Second
		case autoReconnectingTries < 15:
			sleepTime = 1 * time.Second
		case autoReconnectingTries < 25:
			sleepTime = 2 * time.Second
		case autoReconnectingTries < 30:
			sleepTime = 5 * time.Second
		default:
			sleepTime = 8 * time.Second
		}

		if autoReconnectingTries > 15 {
			slog.Info(fmt.Sprintf("Sleeping for %s seconds before auto-reconnecting...", sleepTime.String()))
		}

		time.Sleep(sleepTime)

		if err := Init(); err != nil {
			slog.Error(fmt.Sprintf("%v", err))

			if state == Connected && args.OnDisconnect != "" {
				if err := LoadFileAndPlay(args.OnDisconnect); err != nil {
					slog.Error(fmt.Sprintf("%v", err))
				}
			}
		}

		state = Disconnected

		autoReconnectingTries++
	}

	slog.Info("Lost connection. Exiting...")

}
