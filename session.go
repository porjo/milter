/* milter session */
package milter

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net/textproto"
	"strings"
)

const (
	// negotiation actions
	AddHeader    = 0x01
	ChangeBody   = 0x02
	AddRcpt      = 0x04
	RemoveRcpt   = 0x08
	ChangeHeader = 0x10
	Quarantine   = 0x20

	// undesired protocol content
	NoConnect  = 0x01
	NoHelo     = 0x02
	NoMailFrom = 0x04
	NoRcptTo   = 0x08
	NoBody     = 0x10
	NoHeaders  = 0x20
	NoEOH      = 0x40
)

/* Milter represents incoming milter command */
type MilterSession struct {
	Actions  uint32
	Protocol uint32
	Sock     io.ReadWriteCloser
	Headers  textproto.MIMEHeader
	Macros   map[string]string
	Body     []byte
	Milter   Milter
}

/* ReadPacket reads incoming milter packet */
func (c *MilterSession) ReadPacket() (*Message, error) {
	// read packet length
	var length uint32
	if err := binary.Read(c.Sock, binary.BigEndian, &length); err != nil {
		return nil, err
	}

	// read packet data
	data := make([]byte, length)
	if _, err := io.ReadFull(c.Sock, data); err != nil {
		return nil, err
	}

	// prepare response data
	message := Message{
		Code: data[0],
		Data: data[1:],
	}

	return &message, nil
}

/* WritePacket sends a milter response packet to socket stream */
func (m *MilterSession) WritePacket(msg *Message) error {
	buffer := bufio.NewWriter(m.Sock)

	// calculate and write response length
	length := uint32(len(msg.Data) + 1)
	if err := binary.Write(buffer, binary.BigEndian, length); err != nil {
		return err
	}

	// write response code
	if err := buffer.WriteByte(msg.Code); err != nil {
		return err
	}

	// write response data
	if _, err := buffer.Write(msg.Data); err != nil {
		return err
	}

	// flush data to network socket stream
	if err := buffer.Flush(); err != nil {
		return err
	}

	return nil
}

/* Process milter message / command */
func (m *MilterSession) Process(msg *Message) (Response, error) {
	switch msg.Code {
	case 'A':
		// abort current message and start over
		m.Headers = nil
		m.Body = nil
		m.Macros = nil
		// do not send response
		return nil, nil

	case 'B':
		// body chunk, store data in buffer
		m.Body = append(m.Body, msg.Data...)
		return m.Milter.BodyChunk(msg.Data, NewModifier(m))

	case 'C':
		// new connection, get hostname
		Hostname := ReadCString(msg.Data)
		msg.Data = msg.Data[len(Hostname)+1:]
		// get protocol family
		ProtocolFamily := msg.Data[0]
		msg.Data = msg.Data[1:]
		// get port
		Port := binary.BigEndian.Uint16(msg.Data)
		msg.Data = msg.Data[2:]
		// get address
		Address := ReadCString(msg.Data)
		// convert address and port to human readable string
		var family, address string
		switch ProtocolFamily {
		case 'U':
			family = "unknown"
			address = Address
		case 'L':
			family = "unix"
			address = Address
		case '4':
			family = "tcp4"
			address = fmt.Sprintf("%s:%d", Address, Port)
		case '6':
			family = "tcp6"
			address = fmt.Sprintf("[%s]:%d", Address, Port)
		}
		// run handler and return
		return m.Milter.Connect(Hostname, family, address, NewModifier(m))

	case 'D':
		// define macros
		m.Macros = make(map[string]string)
		// convert data to golang strings
		data := DecodeCStrings(msg.Data[1:])
		if len(data) == 0 {
			// store data in a map
			for i := 0; i < len(data); i += 2 {
				m.Macros[data[i]] = data[i+1]
			}
		}
		// do not send response
		return nil, nil

	case 'E':
		// call and return milter handler
		return m.Milter.Body(m.Body, NewModifier(m))

	case 'H':
		// helo command
		name := strings.TrimSuffix(string(msg.Data), NULL)
		return m.Milter.Helo(name, NewModifier(m))

	case 'L':
		// make sure Headers is initialized
		if m.Headers == nil {
			m.Headers = make(textproto.MIMEHeader)
		}
		// add new header to headers map
		HeaderData := DecodeCStrings(msg.Data)
		if len(HeaderData) == 2 {
			m.Headers.Add(HeaderData[0], HeaderData[1])
			// call and return milter handler
			return m.Milter.Header(HeaderData[0], HeaderData[1], NewModifier(m))
		}

	case 'M':
		// envelope from address
		envfrom := string(msg.Data[0 : len(msg.Data)-1])
		return m.Milter.MailFrom(strings.Trim(envfrom, "<>"), NewModifier(m))

	case 'N':
		// end of headers
		return m.Milter.Headers(m.Headers, NewModifier(m))

	case 'O':
		// ignore request and prepare response buffer
		buffer := new(bytes.Buffer)
		// prepare response data
		for _, value := range []uint32{2, m.Actions, m.Protocol} {
			if err := binary.Write(buffer, binary.BigEndian, value); err != nil {
				return nil, err
			}
		}
		// build and send packet
		return NewResponse('O', buffer.Bytes()), nil

	case 'Q':
		// client requested session close
		return nil, ECloseSession

	case 'R':
		// envelope to address
		envto := string(msg.Data[0 : len(msg.Data)-1])
		return m.Milter.RcptTo(strings.Trim(envto, "<>"), NewModifier(m))

	case 'T':
		// data, ignore

	default:
		// print error and close session
		log.Printf("Unrecognized command code: %c", msg.Code)
		return nil, ECloseSession
	}

	// by default continue with next milter message
	return RespContinue, nil
}

/* process all milter commands in the same connection */
func (m *MilterSession) HandleMilterCommands() {
	// close session socket on exit
	defer m.Sock.Close()

	for {
		// ReadPacket
		msg, err := m.ReadPacket()
		if err != nil {
			if err != io.EOF {
				log.Printf("Error reading milter command: %v", err)
			}
			return
		}

		// process command
		resp, err := m.Process(msg)
		if err != nil {
			if err != ECloseSession {
				// log error condition
				log.Printf("Error performing milter command: %v", err)
			}
			return
		}

		// ignore empty responses
		if resp != nil {
			// send back response message
			if err = m.WritePacket(resp.Response()); err != nil {
				log.Printf("Error writing packet: %v", err)
				return
			}

			if !resp.Continue() {
				return
			}

		}
	}
}
