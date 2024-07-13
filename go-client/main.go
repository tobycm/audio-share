package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/alexflint/go-arg"
	"github.com/ebitengine/oto/v3"
	"google.golang.org/protobuf/proto"

	"tobycm.dev/audio-share-go-client/pb"
)

var audioFormat = &pb.AudioFormat{}

var autoReconnectingTries = 0

var args struct {
	Host          string `arg:"positional,required" help:"Host to connect to."`
	Port          int    `arg:"positional" default:"65530" help:"Port to connect to."`
	Verbose       bool   `arg:"-v" help:"Verbose output."`
	AutoReconnect bool   `arg:"-r" help:"Automatically reconnect on connection loss."`
	TcpTimeout    int    `arg:"-t" default:"3000" help:"TCP timeout in seconds."`
}

var otoCtx *oto.Context

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
	player.Play()

	return handleIncomingTCPData(&tcpConn)
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

	err := Init()
	if err != nil {
		slog.Error(fmt.Sprintf("%v", err))
	}

	for args.AutoReconnect {

		sleepTime := 0 * time.Second

		switch {
		case autoReconnectingTries == 0:
			sleepTime = 0 * time.Second
		case autoReconnectingTries < 2:
			sleepTime = 1 * time.Second
		case autoReconnectingTries < 5:
			sleepTime = 2 * time.Second
		case autoReconnectingTries < 10:
			sleepTime = 5 * time.Second
		default:
			sleepTime = 8 * time.Second
		}

		slog.Info(fmt.Sprintf("Sleeping for %s seconds before auto-reconnecting...", sleepTime.String()))

		time.Sleep(sleepTime)

		err = Init()
		if err != nil {
			slog.Error(fmt.Sprintf("%v", err))
		}

		autoReconnectingTries++
	}

	slog.Info("Lost connection. Exiting...")

}
