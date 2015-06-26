package mongodb

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/elastic/libbeat/common"
	"github.com/elastic/libbeat/logp"

	"labix.org/v2/mgo/bson"
)

func mongodbMessageParser(s *MongodbStream) (bool, bool) {
	d := newDecoder(s.data)

	length, err := d.readInt32()

	if err != nil {
		// Not even enough data to parse length of message
		return true, false
	}

	if length > len(s.data) {
		// Not yet reached the end of message
		return true, false
	}

	// Tell decoder to only consider current message
	d.truncate(length)

	// fill up the header common to all messages
	// see http://docs.mongodb.org/meta-driver/latest/legacy/mongodb-wire-protocol/#standard-message-header
	s.message.messageLength = length
	s.message.requestId, err = d.readInt32()
	s.message.responseTo, err = d.readInt32()
	opCode, err := d.readInt32()

	if _, present := OpCodes[opCode]; !present {
		logp.Err("Unknown operation code: %s", opCode)
		return false, false
	}
	s.message.opCode = OpCodes[opCode]
	s.message.IsResponse = false // default is that the message is a request. If not opReplyParse will set this to false
	s.message.ExpectsResponse = false
	logp.Debug("mongodb", "opCode = "+s.message.opCode)

	// then split depending on operation type
	s.message.event = common.MapStr{}

	switch s.message.opCode {
	case "OP_REPLY":
		s.message.IsResponse = true
		return opReplyParse(d, s.message)
	case "OP_MSG":
		s.message.method = "msg"
		return opMsgParse(d, s.message)
	case "OP_UPDATE":
		s.message.method = "update"
		return opUpdateParse(d, s.message)
	case "OP_INSERT":
		s.message.method = "insert"
		return opInsertParse(d, s.message)
	case "OP_QUERY":
		s.message.ExpectsResponse = true
		return opQueryParse(d, s.message)
	case "OP_GET_MORE":
		s.message.method = "getMore"
		s.message.ExpectsResponse = true
		return opGetMoreParse(d, s.message)
	case "OP_DELETE":
		s.message.method = "delete"
		return opDeleteParse(d, s.message)
	case "OP_KILL_CURSORS":
		s.message.method = "killCursors"
		return opKillCursorsParse(d, s.message)
	}

	return false, false
}

// see http://docs.mongodb.org/meta-driver/latest/legacy/mongodb-wire-protocol/#op-reply
func opReplyParse(d *decoder, m *MongodbMessage) (bool, bool) {
	_, err := d.readInt32() // ignore flags for now
	m.event["cursorId"], err = d.readInt64()
	m.event["startingFrom"], err = d.readInt32()

	numberReturned, err := d.readInt32()
	m.event["numberReturned"] = numberReturned

	logp.Debug("mongodb", "Prepare to read %i document from reply", m.event["numberReturned"])

	documents := make([]string, numberReturned)
	for i := 0; i < numberReturned; i++ {
		var document bson.M
		document, err = d.readDocument()

		// Check if the result is actually an error
		if i == 0 {
			if mongoError, present := document["$err"]; present {
				m.error, err = doc2str(mongoError)
			}
		}

		documents[i], err = doc2str(document)
	}
	m.event["documents"] = documents

	if err != nil {
		logp.Err("An error occured while parsing OP_REPLY message: %s", err)
		return false, false
	}
	return true, true
}

func opMsgParse(d *decoder, m *MongodbMessage) (bool, bool) {
	var err error
	m.event["message"], err = d.readCStr()
	if err != nil {
		logp.Err("An error occured while parsing OP_MSG message: %s", err)
		return false, false
	}
	return true, true
}

func opUpdateParse(d *decoder, m *MongodbMessage) (bool, bool) {
	_, err := d.readInt32() // always ZERO, a slot reserved in the protocol for future use
	m.event["fullCollectionName"], err = d.readCStr()
	_, err = d.readInt32() // ignore flags for now

	m.event["selector"], err = d.readDocumentStr()
	m.event["update"], err = d.readDocumentStr()

	if err != nil {
		logp.Err("An error occured while parsing OP_UPDATE message: %s", err)
		return false, false
	}

	return true, true
}

func opInsertParse(d *decoder, m *MongodbMessage) (bool, bool) {
	_, err := d.readInt32() // ignore flags for now
	m.event["fullCollectionName"], err = d.readCStr()

	// TODO parse bson documents
	// Not too bad if it is not done, as all recent mongodb clients send insert as a command over a query instead of this
	// Find an old client to generate a pcap with legacy protocol ?

	if err != nil {
		logp.Err("An error occured while parsing OP_INSERT message: %s", err)
		return false, false
	}

	return true, true
}

func opQueryParse(d *decoder, m *MongodbMessage) (bool, bool) {
	_, err := d.readInt32() // ignore flags for now
	fullCollectionName, err := d.readCStr()
	m.event["fullCollectionName"] = fullCollectionName

	m.event["numberToSkip"], err = d.readInt32()
	m.event["numberToReturn"], err = d.readInt32()

	query, err := d.readDocument()
	if d.i < len(d.in) {
		m.event["returnFieldsSelector"], err = d.readDocumentStr()
	}

	// Actual method is either a 'find' or a command passing through a query
	if strings.HasSuffix(fullCollectionName, ".$cmd") {
		m.method = "otherCommand"
		for _, command := range UserCommands {
			if _, present := query[command]; present {
				m.method = command
			}
		}
	} else {
		m.method = "find"
	}

	m.event["query"], err = doc2str(query)

	if err != nil {
		logp.Err("An error occured while parsing OP_QUERY message: %s", err)
		return false, false
	}

	return true, true
}

func opGetMoreParse(d *decoder, m *MongodbMessage) (bool, bool) {
	_, err := d.readInt32() // always ZERO, a slot reserved in the protocol for future use
	m.event["fullCollectionName"], err = d.readCStr()
	m.event["numberToReturn"], err = d.readInt32()
	m.event["cursorId"], err = d.readInt64()

	if err != nil {
		logp.Err("An error occured while parsing OP_GET_MORE message: %s", err)
		return false, false
	}
	return true, true
}

func opDeleteParse(d *decoder, m *MongodbMessage) (bool, bool) {
	_, err := d.readInt32() // always ZERO, a slot reserved in the protocol for future use
	m.event["fullCollectionName"], err = d.readCStr()
	_, err = d.readInt32() // ignore flags for now

	m.event["selector"], err = d.readDocumentStr()

	if err != nil {
		logp.Err("An error occured while parsing OP_DELETE message: %s", err)
		return false, false
	}

	return true, true
}

func opKillCursorsParse(d *decoder, m *MongodbMessage) (bool, bool) {
	// TODO ? Or not, content is not very interesting.
	return true, true
}

// NOTE: The following functions are inspired by the source of the go-mgo/mgo project
// https://github.com/go-mgo/mgo/blob/v2/bson/decode.go

type decoder struct {
	in []byte
	i  int
}

func newDecoder(in []byte) *decoder {
	return &decoder{in, 0}
}

func (d *decoder) truncate(length int) {
	d.in = d.in[:length]
}

func (d *decoder) readCStr() (string, error) {
	start := d.i
	end := start
	l := len(d.in)
	for ; end != l; end++ {
		if d.in[end] == '\x00' {
			break
		}
	}
	d.i = end + 1
	if d.i > l {
		return "", errors.New("cstring not finished")
	}
	return string(d.in[start:end]), nil
}

func (d *decoder) readInt32() (int, error) {
	b, err := d.readBytes(4)

	if err != nil {
		return 0, err
	}

	return int((uint32(b[0]) << 0) |
		(uint32(b[1]) << 8) |
		(uint32(b[2]) << 16) |
		(uint32(b[3]) << 24)), nil
}

func (d *decoder) readInt64() (int, error) {
	b, err := d.readBytes(8)

	if err != nil {
		return 0, err
	}

	return int((uint64(b[0]) << 0) |
		(uint64(b[1]) << 8) |
		(uint64(b[2]) << 16) |
		(uint64(b[3]) << 24) |
		(uint64(b[4]) << 32) |
		(uint64(b[5]) << 40) |
		(uint64(b[6]) << 48) |
		(uint64(b[7]) << 56)), nil
}

func (d *decoder) readDocument() (bson.M, error) {
	start := d.i
	documentLength, err := d.readInt32()
	d.i = start + documentLength

	documentMap := bson.M{}

	logp.Debug("mongodb", "Parse %d bytes document from remaining %d bytes", documentLength, len(d.in)-start)
	err = bson.Unmarshal(d.in[start:d.i], documentMap)

	if err != nil {
		logp.Debug("mongodb", "Unmarshall error %v", err)
		return nil, err
	}

	return documentMap, err
}

func doc2str(documentMap interface{}) (string, error) {
	document, err := json.Marshal(documentMap)
	return string(document), err
}

func (d *decoder) readDocumentStr() (string, error) {
	documentMap, err := d.readDocument()
	document, err := doc2str(documentMap)
	return document, err
}

func (d *decoder) readBytes(length int32) ([]byte, error) {
	start := d.i
	d.i += int(length)
	if d.i > len(d.in) {
		return *new([]byte), errors.New("No byte to read")
	}
	return d.in[start : start+int(length)], nil
}