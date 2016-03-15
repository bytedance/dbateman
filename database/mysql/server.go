// Copyright 2016 ByteDance, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package mysql

import (
	"bytes"
	"encoding/binary"
	"fmt"
	_ "github.com/bytedance/dbatman/database/sql/driver"
	"github.com/juju/errors"
	"net"
)

// MySQLServer is a server-side interface of 1-time-connection
//
type MySQLServer interface {
	ConnID() uint32
	Salt() []byte
	Collation() uint8
	Status() uint16

	Cap() uint32
	SetCap(c uint32)

	ResetSequence()

	CheckAuth(user string, auth []byte, db string) error

	DefaultDB() string
	ServerName() []byte
}

// Connection between mysql client <-> mysql server
// here we wrap the go-mysql-driver.mysqlConn
type MySQLServerConn struct {
	*mysqlConn
	MySQLServer
}

// Hnadshake init handshake package to the client, wait for client autheticate
// response.
func Handshake(s MySQLServer, conn net.Conn) (*MySQLServerConn, error) {
	c := new(MySQLServerConn)
	var err error = nil

	c.MySQLServer = s

	c.mysqlConn = &mysqlConn{
		maxPacketAllowed: maxPacketSize,
		maxWriteSize:     maxPacketSize - 1,
		netConn:          conn,
	}

	c.buf = newBuffer(c.netConn)

	// Handeshake
	if err = c.writeInitPacket(); err != nil {
		c.cleanup()
		return nil, errors.Trace(err)
	}

	if err = c.readHandshakeResponse(); err != nil {
		c.WriteError(err)
		c.cleanup()
		return nil, errors.Trace(err)
	}

	// TODO here we should proceed PROTOCOL41 ?
	if err = c.WriteOK(nil); err != nil {
		c.cleanup()
		return nil, errors.Trace(err)
	}

	c.ResetSequence()

	return c, err
}

/******************************************************************************
*                          Server-Side MySQL Error                            *
******************************************************************************/

// SqlError is used for server-side, it represent a error during mysql connect or
// query phase
type SqlError struct {
	*MySQLError
	State string
}

func NewSqlError(errno uint16, args ...interface{}) *SqlError {

	e := &SqlError{
		MySQLError: &MySQLError{},
	}
	e.Number = errno

	if s, ok := MySQLState[errno]; ok {
		e.State = s
	} else {
		e.State = DEFAULT_MYSQL_STATE
	}

	if format, ok := MySQLErrName[errno]; ok {
		e.Message = fmt.Sprintf(format, args...)
	} else {
		e.Message = fmt.Sprint(args...)
	}

	return e

}

/******************************************************************************
*                   Server-Side Initialisation Process                        *
******************************************************************************/

// Handshake Initialization Packet
// http://dev.mysql.com/doc/internals/en/connection-phase-packets.html#packet-Protocol::Handshake
func (mc *MySQLServerConn) writeInitPacket() error {
	// preserved for write head
	data := make([]byte, 4, 128)

	// min version 10
	data = append(data, 10)

	// server version[00]
	data = append(data, mc.ServerName()...)
	data = append(data, 0)

	// connection id
	data = append(data, byte(mc.ConnID()), byte(mc.ConnID()>>8), byte(mc.ConnID()>>16), byte(mc.ConnID()>>24))

	// auth-plugin-data-part-1
	data = append(data, mc.Salt()[0:8]...)

	// filter [00]
	data = append(data, 0)

	// capability flag lower 2 bytes, using default capability here
	data = append(data, byte(mc.Cap()), byte(mc.Cap()>>8))

	// charset, utf-8 default
	data = append(data, uint8(mc.Collation()))

	// status
	data = append(data, byte(mc.Status()), byte(mc.Status()>>8))

	// below 13 byte may not be used
	// capability flag upper 2 bytes, using default capability here
	data = append(data, byte(mc.Cap()>>16), byte(mc.Cap()>>24))

	// filter [0x15], for wireshark dump, value is 0x15
	data = append(data, 0x00)

	// reserved 10 [00]
	data = append(data, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0)

	// auth-plugin-data-part-2
	data = append(data, mc.Salt()[8:]...)

	// filter [00]
	data = append(data, 0)

	if err := mc.writePacket(data); err != nil {
		return err
	}

	return nil
}

// ReadHandshakeResponse read the client handshake response, set the collations and
// capability, check the authetication info.
// for futher infomation, read the doc:
// http://dev.mysql.com/doc/internals/en/connection-phase-packets.html#packet-Protocol::Handshake
func (mc *MySQLServerConn) readHandshakeResponse() error {
	data, err := mc.readPacket()
	if err != nil {
		return err
	}

	pos := 0

	//capability
	mc.SetCap(binary.LittleEndian.Uint32(data[:4]))
	pos += 4

	//skip max packet size
	pos += 4

	//charset, skip, if you want to use another charset, use set names
	//c.collation = CollationId(data[pos])
	pos++

	//skip reserved 23[00]
	pos += 23

	//user name
	user := string(data[pos : pos+bytes.IndexByte(data[pos:], 0)])
	pos += len(user) + 1

	//auth length and auth
	authLen := int(data[pos])
	pos++
	auth := data[pos : pos+authLen]
	pos += authLen

	if mc.Cap()&uint32(clientConnectWithDB) == 0 {
		if err := mc.CheckAuth(user, auth, ""); err != nil {
			return err
		}
	} else {
		// connect must with db, otherwise it will deny the access
		if len(data[pos:]) == 0 {
			return errors.Trace(NewSqlError(ER_ACCESS_DENIED_ERROR, mc.netConn.RemoteAddr().String(), user, "Yes"))
		}

		db := string(data[pos : pos+bytes.IndexByte(data[pos:], 0)])
		pos += len(db) + 1

		// check with user
		if err := mc.CheckAuth(user, auth, db); err != nil {
			return err
		}
	}

	return nil
}

/******************************************************************************
*                   Function Send Packets to front client                     *
******************************************************************************/

type MySQLResult struct {
	*mysqlResult
	Status   uint16
	Warnings uint16
}

// WriteError write error package to the client
func (mc *MySQLServerConn) WriteError(e error) error {
	var m *SqlError
	var ok bool
	if m, ok = e.(*SqlError); !ok {
		m = NewSqlError(ER_UNKNOWN_ERROR, e.Error())
	}

	data := mc.buf.takeSmallBuffer(16 + len(m.Message))

	data = append(data, ERR)
	data = append(data, byte(m.Number), byte(m.Number>>8))

	if mc.Cap()&uint32(clientProtocol41) > 0 {
		data = append(data, '#')
		data = append(data, m.State...)
	}

	data = append(data, m.Message...)

	return mc.writePacket(data)
}

// WriteOk write ok package to the client
func (mc *MySQLServerConn) WriteOK(r *MySQLResult) error {
	if r == nil {
		r = &MySQLResult{Status: mc.Status()}
	}
	data := mc.buf.takeSmallBuffer(32)

	data = append(data, OK)

	rows, _ := r.RowsAffected()
	insertId, _ := r.LastInsertId()
	data = append(data, PutLengthEncodedInt(uint64(rows))...)
	data = append(data, PutLengthEncodedInt(uint64(insertId))...)

	if mc.Cap()&uint32(clientProtocol41) > 0 {
		data = append(data, byte(r.Status), byte(r.Status>>8))
		data = append(data, byte(r.Warnings), byte(r.Warnings>>8))
	}

	return mc.writePacket(data)
}

/******************************************************************************
*                   Function Wrapper for Export Visiable                      *
******************************************************************************/

func (mc *mysqlConn) HandleOkPacket(data []byte) error {
	return mc.handleOkPacket(data)
}

func (mc *mysqlConn) HandleErrorPacket(data []byte) error {
	return mc.handleErrorPacket(data)
}