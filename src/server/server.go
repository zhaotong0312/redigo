package server

import (
	. "redigo/src/structure"
	. "redigo/src/constant"
	. "redigo/src/networking"
	"sync"
	"fmt"
	"strconv"
	"bytes"
	"strings"
	"net"
	"time"
	"os"
	"os/signal"
	"syscall"
	"io/ioutil"
	"errors"
	"path/filepath"
)

type ClientBufferLimitsConfig struct {
	HardLimitBytes        int64
	SoftLimitBytes        int64
	SoftLimitTimeDuration time.Duration
}

type Server struct {
	Pid                  int64
	PidFile              string
	ConfigFile           string
	ExecFile             string
	ExecArgv             []string
	Hz                   int64 // serverCron() calls frequency in hertz
	Dbs                  []*Db
	DbNum                int64
	Commands             map[string]*Command
	OrigCommands         map[string]*Command
	UnixTime             time.Time // UnixTime in millisecond
	LruClock             int64     // Clock for LRU eviction
	CronLoops            int64
	NextClientId         int64
	Port                 int64 // TCP listening port
	BindAddrs            []string
	BindAddrCount        int64  // Number of addresses in server.bindaddr[]
	UnixSocketPath       string // UNIX socket path
	CurrentClient        *Client
	Clients              *List // List of active clients
	ClientsMap           map[int64]*Client
	ClientsToClose       *List // Clients to close asynchronously
	ClientMaxQueryBufLen int64
	MaxClients           int64
	ProtectedMode        bool // Don't accept external connections.
	RequirePassword      *string
	TcpKeepAlive         bool
	ProtoMaxBulkLen      int64
	ClientsPaused        bool
	ClientsPauseEndTime  time.Time
	Dirty                int64 // Changes to DB from the last save
	Shared               *SharedObjects
	StatRejectedConn     int64
	StatConnCount        int64
	StatNetOutputBytes   int64
	StatNetInputBytes    int64
	StatNumCommands      int64
	ConfigFlushAll       bool
	mutex                sync.RWMutex
	MaxMemory            int64
	Loading              bool
	CloseCh              chan struct{}
	LogLevel             int64
	//AlsoPropagate        *OpArray
	//ClientObufLimits     []ClientBufferLimitsConfig
}

func (s *Server) CreateClient(conn net.Conn) *Client {
	if conn != nil {
		if s.TcpKeepAlive {
			AnetSetTcpKeepALive(conn.(*net.TCPConn), s.TcpKeepAlive)
		}
	}
	createTime := s.UnixTime
	c := Client{
		Id:              0,
		Conn:            conn,
		Name:            "",
		QueryBuf:        make([]byte, PROTO_INLINE_MAX_SIZE),
		QueryBufPeak:    0,
		Argc:            0,                 // count of arguments
		Argv:            make([]string, 0), // arguments of current command
		Cmd:             nil,
		LastCmd:         nil,
		Reply:           ListCreate(),
		ReplySize:       0,
		CreateTime:      createTime,
		LastInteraction: createTime,
		Buf:             make([]byte, PROTO_REPLY_CHUNK_BYTES),
		BufPos:          0,
		SentLen:         0,
		Flags:           0,
		Node:            nil,
		PeerId:          "",
		RequestType:     0,
		MultiBulkLen:    0,
		BulkLen:         0,
		Authenticated:   0,
		ReadCh:          make(chan struct{}, 1),
		WriteCh:         make(chan struct{}, 1),
		CloseCh:         make(chan struct{}, 1),
		//PendingWriteNode: nil,
		//UnblockedNode:    nil,
	}
	c.GetNextClientId(s)
	SelectDB(s, &c, 0)
	s.LinkClient(&c)
	return &c
}

func (s *Server) LinkClient(c *Client) {
	s.Clients.ListAddNodeTail(c)
	s.ClientsMap[c.Id] = c
	c.Node = s.Clients.ListTail()
	s.StatConnCount++
}

func (s *Server) UnLinkClient(c *Client) {
	if s.CurrentClient == c {
		s.CurrentClient = nil
	}
	if c.Conn != nil {
		s.Clients.ListDelNode(c.Node)
		c.Node = nil
		delete(s.ClientsMap, c.Id)
		s.StatConnCount--
		c.Conn.Close()
		c.Conn = nil
	}
	//if c.WithFlags(CLIENT_PENDING_WRITE) {
	//	s.ClientsPendingWrite.ListDelNode(c.PendingWriteNode)
	//	c.PendingWriteNode = nil
	//	c.DeleteFlags(CLIENT_PENDING_WRITE)
	//}
	//if c.WithFlags(CLIENT_UNBLOCKED) {
	//	s.ClientsUnblocked.ListDelNode(c.UnblockedNode)
	//	c.UnblockedNode = nil
	//	c.DeleteFlags(CLIENT_UNBLOCKED)
	//}
}

func CloseClient(s *Server, c *Client) {
	fmt.Println("CloseClient")
	c.QueryBuf = nil
	//c.PendingWriteNode = nil
	c.Reply.ListEmpty()
	c.Reply = nil
	c.ResetArgv()
	s.UnLinkClient(c)
	if c.WithFlags(CLIENT_CLOSE_ASAP) {
		ln := s.ClientsToClose.ListSearchKey(c)
		s.ClientsToClose.ListDelNode(ln)
	}
	close(c.CloseCh)
	close(c.ReadCh)
	close(c.WriteCh)
}

func CloseClientAsync(s *Server, c *Client) {
	if c.WithFlags(CLIENT_CLOSE_ASAP) {
		return
	}
	c.AddFlags(CLIENT_CLOSE_ASAP)
	s.ClientsToClose.ListAddNodeTail(c)
}

func CloseClientsInAsyncList(s *Server) {
	fmt.Println("CloseClientsInAsyncList")
	for s.ClientsToClose.ListLength() != 0 {
		ln := s.ClientsToClose.ListHead()
		c := ln.Value.(*Client)
		c.DeleteFlags(CLIENT_CLOSE_ASAP)
		CloseClient(s, c)
		s.ClientsToClose.ListDelNode(ln)
	}
}

func (s *Server) GetClientById(id int64) *Client {
	return s.ClientsMap[id]
}

func PrepareClientToWrite(c *Client) int64 {
	//if c.WithFlags(CLIENT_LUA | CLIENT_MODULE) {
	//	return C_OK
	//}

	if c.WithFlags(CLIENT_REPLY_OFF | CLIENT_REPLY_SKIP) {
		return C_ERR
	}

	//if c.WithFlags(CLIENT_MASTER) && !c.WithFlags(CLIENT_MASTER_FORCE_REPLY) {
	//	return C_ERR
	//}
	if c.Conn == nil {
		// Fake client for AOF loading.
		return C_ERR
	}

	//if !c.HasPendingReplies() && !(c.WithFlags(CLIENT_PENDING_WRITE)) {
	//	c.AddFlags(CLIENT_PENDING_WRITE)
	//	s.ClientsPendingWrite.ListAddNodeTail(c)
	//}
	return C_OK
}

func (s *Server) AddReplyToBuffer(c *Client, str string) int64 {
	if c.WithFlags(CLIENT_CLOSE_AFTER_REPLY) {
		return C_OK
	}
	if c.Reply.ListLength() > 0 {
		return C_ERR
	}
	available := cap(c.Buf)
	if len(str) > available {
		return C_ERR
	}
	copy(c.Buf[c.BufPos:], str)
	c.BufPos += int64(len(str))
	return C_OK
}

func (s *Server) AddReplyToList(c *Client, str string) {
	if c.WithFlags(CLIENT_CLOSE_AFTER_REPLY) {
		return
	}
	if c.Reply.ListLength() == 0 {
		c.Reply.ListAddNodeTail(&str)
		c.ReplySize += int64(len(str))
	} else {
		ln := c.Reply.ListTail()
		tail := *ln.Value.(*string)
		if tail != "" && (len(tail) >= len(str) || len(tail)+len(str) < PROTO_REPLY_CHUNK_BYTES) {
			tail = CatString(tail, str)
			ln.Value = &tail
			c.ReplySize += int64(len(str))
		} else {
			c.Reply.ListAddNodeTail(&str)
			c.ReplySize += int64(len(str))
		}

	}
	//AsyncCloseClientOnOutputBufferLimitReached(s, c)
}

func (s *Server) AddReply(c *Client, str string) {
	if PrepareClientToWrite(c) != C_OK {
		return
	}
	if s.AddReplyToBuffer(c, str) != C_OK {
		s.AddReplyToList(c, str)
	}
}

func (s *Server) AddReplyStrObj(c *Client, o *StrObject) {
	if !CheckRType(o, OBJ_RTYPE_STR) {
		return
	}
	str, err := GetStrObjectValueString(o)
	if err == nil {
		s.AddReply(c, str)
	} else {
		return
	}
}

func (s *Server) AddReplyError(c *Client, str string) {
	if len(str) != 0 || str[0] != '-' {
		s.AddReply(c, "-ERR ")
	}
	s.AddReply(c, str)
	s.AddReply(c, "\r\n")
}

func (s *Server) AddReplyErrorFormat(c *Client, format string, a ...interface{}) {
	str := fmt.Sprintf(format, a)
	s.AddReplyError(c, str)
}

func (s *Server) AddReplyStatus(c *Client, str string) {
	s.AddReply(c, "+")
	s.AddReply(c, str)
	s.AddReply(c, "\r\n")
}

func (s *Server) AddReplyStatusFormat(c *Client, format string, a ...interface{}) {
	str := fmt.Sprintf(format, a)
	s.AddReplyStatus(c, str)
}

//func (s *Server) AddReplyHelp(c *Client, help []string) {
//	cmd := c.Argv[0]
//	s.AddReplyStatusFormat(c, "%s <subcommand> arg arg ... arg. Subcommands are:", cmd)
//	for _, h := range help {
//		s.AddReplyStatus(c, h)
//	}
//}

func (s *Server) AddReplyIntWithPrifix(c *Client, i int64, prefix byte) {
	/* Things like $3\r\n or *2\r\n are emitted very often by the protocol
	so we have a few shared objects to use if the integer is small
	like it is most of the times. */
	if prefix == '*' && i >= 0 && i < SHARED_BULKHDR_LEN {
		s.AddReply(c, s.Shared.MultiBulkHDR[i])
	} else if prefix == '$' && i >= 0 && i < SHARED_BULKHDR_LEN {
		s.AddReply(c, s.Shared.MultiBulkHDR[i])
	} else {
		str := strconv.FormatInt(i, 10)
		buf := bytes.Buffer{}
		buf.WriteByte(prefix)
		buf.WriteString(str)
		buf.WriteByte('\r')
		buf.WriteByte('\n')
		s.AddReply(c, buf.String())
	}
}

func (s *Server) AddReplyInt(c *Client, i int64) {
	if i == 0 {
		s.AddReply(c, s.Shared.Zero)
	} else if i == 1 {
		s.AddReply(c, s.Shared.One)
	} else {
		s.AddReplyIntWithPrifix(c, i, ':')
	}
}

func (s *Server) AddReplyMultiBulkLength(c *Client, length int64) {
	s.AddReplyIntWithPrifix(c, length, '*')
}

/* Create the length prefix of a bulk reply, example: $2234 */
func (s *Server) AddReplyBulkLengthString(c *Client, str string) {
	length := int64(len(str))
	s.AddReplyIntWithPrifix(c, length, '$')
}

func (s *Server) AddReplyBulkLengthStrObj(c *Client, o *StrObject) {
	if !CheckRType(o, OBJ_RTYPE_STR) {
		return
	}
	str, err := GetStrObjectValueString(o)
	if err == nil {
		s.AddReplyBulkLengthString(c, str)
	} else {
		return
	}
}

func (s *Server) AddReplyBulk(c *Client, o *StrObject) {
	s.AddReplyBulkLengthStrObj(c, o)
	s.AddReplyStrObj(c, o)
	s.AddReply(c, s.Shared.Crlf)
}

func (s *Server) AddReplyBulkString(c *Client, str string) {
	if str == "" {
		s.AddReply(c, s.Shared.NullBulk)
	} else {
		s.AddReplyBulkLengthString(c, str)
		s.AddReply(c, str)
		s.AddReply(c, s.Shared.Crlf)
	}
}

func (s *Server) AddReplyBulkInt(c *Client, i int64) {
	str := strconv.FormatInt(i, 10)
	s.AddReplyBulkString(c, str)
}

func (s *Server) AddReplySubcommandSyntaxError(c *Client) {
	cmd := c.Argv[0]
	s.AddReplyErrorFormat(c, "Unknown subcommand or wrong number of arguments for '%s'. Try %s HELP.", cmd, strings.ToUpper(cmd))
}

// Write data in output buffers to client.
func WriteToClient(s *Server, c *Client) {
	written := int64(0)
	for c.HasPendingReplies() {
		if c.BufPos > 0 {
			n, err := c.Write(c.Buf)
			if err == nil {
				if n <= 0 {
					break
				}
				c.SentLen += int64(n)
				written += n
			}
			if c.SentLen == c.BufPos {
				c.SentLen = 0
				c.BufPos = 0
			}
		} else {
			str := c.Reply.ListHead().Value.(*string)
			length := int64(len(*str))
			if length == 0 {
				c.Reply.ListDelNode(c.Reply.ListHead())
			}
			n, err := c.Write([]byte(*str))
			if err == nil {
				if n <= 0 {
					break
				}
				c.SentLen += int64(n)
				written += n
			}
			if c.SentLen == length {
				c.Reply.ListDelNode(c.Reply.ListHead())
				c.SentLen = 0
				c.ReplySize -= length
				if c.Reply.ListLength() == 0 {
					c.ReplySize = 0
				}
			}
		}
		if written > NET_MAX_WRITES_PER_EVENT {
			break
		}
	}
	s.StatNetOutputBytes += written
	if written > 0 {
		//if !c.WithFlags(CLIENT_MASTER) {
		//	c.LastInteraction = s.UnixTime
		//}
		c.LastInteraction = s.UnixTime
	}
	if !c.HasPendingReplies() {
		c.SentLen = 0
	}
}

/* Like processMultibulkBuffer(), but for the inline protocol instead of RESP,
 * this function consumes the client query buffer and creates a command ready
 * to be executed inside the client structure. Returns C_OK if the command
 * is ready to be executed, or C_ERR if there is still protocol to read to
 * have a well formed command. The function also returns C_ERR when there is
 * a protocol error: in such a case the client structure is setup to reply
 * with the error and close the connection. */
func ProcessInlineBuffer(s *Server, c *Client) int64 {
	// Search for end of line
	newline := bytes.IndexByte(c.QueryBuf, '\n')

	if newline == -1 {
		if len(c.QueryBuf) > PROTO_INLINE_MAX_SIZE {
			s.AddReplyError(c, "Protocol error: too big inline request")
			s.SetProtocolError("too big inline request", c, 0)
		}
		return C_ERR
	}
	if newline != 0 && newline != len(c.QueryBuf) && c.QueryBuf[newline-1] == '\r' {
		// Handle the \r\n case.
		newline--
	}
	/* Split the input buffer up to the \r\n */
	argvs := SplitArgs(c.QueryBuf[0:newline])
	if argvs == nil {
		s.AddReplyError(c, "Protocol error: unbalanced quotes in request")
		s.SetProtocolError("unbalanced quotes in inline request", c, 0)
		return C_ERR
	}

	// Leave data after the first line of the query in the buffer
	c.QueryBuf = c.QueryBuf[newline+2:]
	if len(argvs) != 0 {
		c.Argc = 0
		c.Argv = make([]string, len(argvs))
	}
	for index, argv := range argvs {
		if argv != "" {
			c.Argv[index] = argv
			c.Argc++
		}
	}
	return C_OK
}

/* Process the query buffer for client 'c', setting up the client argument
 * vector for command execution. Returns C_OK if after running the function
 * the client has a well-formed ready to be processed command, otherwise
 * C_ERR if there is still to read more buffer to get the full command.
 * The function also returns C_ERR when there is a protocol error: in such a
 * case the client structure is setup to reply with the error and close
 * the connection.
 *
 * This function is called if processInputBuffer() detects that the next
 * command is in RESP format, so the first byte in the command is found
 * to be '*'. Otherwise for inline commands processInlineBuffer() is called. */
func ProcessMultiBulkBuffer(s *Server, c *Client) int64 {
	pos := 0
	if c.MultiBulkLen == 0 {
		newline := bytes.IndexByte(c.QueryBuf, '\r')
		if newline < 0 {
			if len(c.QueryBuf) > PROTO_INLINE_MAX_SIZE {
				s.AddReplyError(c, "Protocol error: too big multibulk count request")
				s.SetProtocolError("too big multibulk request", c, 0)
			}
			return C_ERR
		}
		if len(c.QueryBuf)-newline < 2 {
			// end with \r\n, so \r cannot be the last char
			return C_ERR
		}
		if c.QueryBuf[0] != '*' {
			return C_ERR
		}
		nulkNum, err := strconv.Atoi(string(c.QueryBuf[pos+1 : newline]))
		if err != nil || nulkNum > 1024*1024 {
			s.AddReplyError(c, "Protocol error: invalid multibulk length")
			s.SetProtocolError("invalid multibulk length", c, 0)
			return C_ERR
		}
		// pos start of bulks
		pos = newline + 2
		if nulkNum <= 0 {
			c.QueryBuf = c.QueryBuf[pos:]
			return C_OK
		}
		c.MultiBulkLen = int64(nulkNum)
		c.Argv = make([]string, c.MultiBulkLen)
	}
	if c.MultiBulkLen < 0 {
		return C_ERR
	}
	for c.MultiBulkLen > 0 {
		if c.BulkLen == -1 {
			// Read bulk length if unknown
			newline := bytes.IndexByte(c.QueryBuf, '\r')
			if newline < 0 {
				if len(c.QueryBuf) > PROTO_INLINE_MAX_SIZE {
					s.AddReplyError(c, "Protocol error: too big bulk count string")
					s.SetProtocolError("too big bulk count string", c, 0)
					return C_ERR
				}
				break
			}
			if len(c.QueryBuf)-newline < 2 {
				// end with \r\n, so \r cannot be the last char
				break
			}
			if c.QueryBuf[pos] != '$' {
				s.AddReplyError(c, fmt.Sprintf("Protocol error: expected '$', got '%c'", c.QueryBuf[pos]))
				s.SetProtocolError("expected $ but got something else", c, 0)
				return C_ERR
			}
			nulkNum, err := strconv.Atoi(string(c.QueryBuf[pos+1 : newline]))
			if err != nil || int64(nulkNum) > s.ProtoMaxBulkLen {
				s.AddReplyError(c, "Protocol error: invalid bulk length")
				s.SetProtocolError("invalid bulk length", c, 0)
				return C_ERR
			}
			pos = newline + 2
			if nulkNum >= PROTO_MBULK_BIG_ARG {
				/* If we are going to read a large object from network
				 * try to make it likely that it will start at c->querybuf
				 * boundary so that we can optimize object creation
				 * avoiding a large copy of data. */
				c.QueryBuf = c.QueryBuf[pos:]
				qblen := len(c.QueryBuf)
				pos = 0
				if qblen < nulkNum+2 {
					//	the only bulk
					c.QueryBuf = append(c.QueryBuf, make([]byte, nulkNum+2-qblen)...)
				}
				c.BulkLen = int64(nulkNum)
			}
			if int64(len(c.QueryBuf)-pos) < c.BulkLen+2 {
				break
			} else {
				if pos == 0 && c.BulkLen >= PROTO_MBULK_BIG_ARG && int64(len(c.QueryBuf)) == c.BulkLen+2 {
					c.Argv = append(c.Argv, string(c.QueryBuf[pos:c.BulkLen]))
					c.Argc++
				} else {
					c.Argv = append(c.Argv, string(c.QueryBuf[pos:c.BulkLen]))
					pos += int(c.BulkLen + 2)
				}
				c.BulkLen = -1
				c.MultiBulkLen--
			}
		}
	}

	if pos > 0 {
		// trim to pos
		c.QueryBuf = c.QueryBuf[pos:]
	}
	if c.MultiBulkLen == 0 {
		return C_OK
	}
	return C_ERR
}

func ReadQueryFromClient(s *Server, c *Client) {
	bufLen := int64(PROTO_IOBUF_LEN)
	queryLen := int64(len(c.QueryBuf))
	if c.RequestType == PROTO_REQ_MULTIBULK && c.MultiBulkLen != 0 && c.BulkLen != -1 && c.BulkLen >= PROTO_MBULK_BIG_ARG {
		remaining := c.BulkLen + 2 - int64(queryLen)
		if remaining < bufLen {
			bufLen = remaining
		}
	}
	if c.QueryBufPeak < queryLen {
		c.QueryBufPeak = queryLen
	}
	readCount, err := c.Conn.Read(c.QueryBuf)
	if err != nil {
		return
	}
	if readCount == 0 {
		return
	}
	c.LastInteraction = s.UnixTime
	//if c.WithFlags(CLIENT_MASTER) {
	//	c.ReadReplyOff += int64(readCount)
	//}
	s.StatNetInputBytes += int64(readCount)
	if int64(len(c.QueryBuf)) > s.ClientMaxQueryBufLen {
		CloseClient(s, c)
		return
	}
	ProcessInputBuffer(s, c)
	//if !c.WithFlags(CLIENT_MASTER) {
	//	s.ProcessInputBuffer(c)
	//} else {
	//	preOff := c.ReplyOff
	//	s.ProcessInputBuffer(c)
	//	applied := c.ReplyOff - preOff
	//	if applied != 0 {
	//	//	do replication from master to slave
	//	}
	//}
}

//func (s *Server) PauseClients(end time.Duration) {
//	if !s.ClientsPaused || end > s.ClientsPauseEndTime {
//		s.ClientsPauseEndTime = end
//	}
//	s.ClientsPaused = true
//}

//func (s *Server) ClientsArePasued() bool {
//	if s.ClientsPaused && s.ClientsPauseEndTime < s.UnixTime {
//		s.ClientsPaused = false
//		iter := s.Clients.ListGetIterator(ITERATION_DIRECTION_INORDER)
//		for node := iter.ListNext(); node != nil; node = iter.ListNext() {
//			c := node.Value.(*Client)
//			if c.WithFlags(CLIENT_SLAVE | CLIENT_BLOCKED) {
//				continue
//			}
//			c.AddFlags(CLIENT_UNBLOCKED)
//			//s.ClientsUnblocked.ListAddNodeTail(c)
//		}
//	}
//	return s.ClientsPaused
//}

/* This function is called every time, in the client structure 'c', there is
 * more query buffer to process, because we read more data from the socket
 * or because a client was blocked and later reactivated, so there could be
 * pending query buffer, already representing a full command, to process. */
func ProcessInputBuffer(s *Server, c *Client) {
	s.CurrentClient = c
	for len(c.QueryBuf) != 0 {
		//if !c.WithFlags(CLIENT_SLAVE) && s.ClientsArePasued() {
		//	break
		//}
		if c.WithFlags(CLIENT_CLOSE_AFTER_REPLY | CLIENT_CLOSE_ASAP) {
			break
		}
		if c.RequestType == 0 {
			if c.QueryBuf[0] == '*' {
				c.RequestType = PROTO_REQ_MULTIBULK
			} else {
				c.RequestType = PROTO_REQ_INLINE
			}
		}
		if c.RequestType == PROTO_REQ_INLINE {
			if ProcessInlineBuffer(s, c) != C_OK {
				break
			}
		} else if c.RequestType == PROTO_REQ_MULTIBULK {
			if ProcessMultiBulkBuffer(s, c) != C_ERR {
				break
			}
		} else {
			panic("Unknown request type")
		}

		if c.Argc == 0 {
			c.Reset()
		} else {
			if ProcessCommand(s, c) == C_OK {
				//if c.WithFlags(CLIENT_MASTER) && !c.WithFlags(CLIENT_MULTI) {
				//	/* Update the applied replication offset of our master. */
				//	c.ReplyOff = c.ReadReplyOff - int64(len(c.QueryBuf))
				//}
				//if !c.WithFlags(CLIENT_BLOCKED) || c.BType != BLOCKED_MODULE {
				//	c.Reset()
				//}
			}
			if s.CurrentClient == nil {
				break
			}
		}
	}
	s.CurrentClient = nil
}

/* Call() is the core of Redis execution of a command.
 *
 * The following flags can be passed:
 * CMD_CALL_NONE        No flags.
 * CMD_CALL_SLOWLOG     Check command speed and log in the slow log if needed.
 * CMD_CALL_STATS       Populate command stats.
 * CMD_CALL_PROPAGATE_AOF   Append command to AOF if it modified the dataset
 *                          or if the client flags are forcing propagation.
 * CMD_CALL_PROPAGATE_REPL  Send command to salves if it modified the dataset
 *                          or if the client flags are forcing propagation.
 * CMD_CALL_PROPAGATE   Alias for PROPAGATE_AOF|PROPAGATE_REPL.
 * CMD_CALL_FULL        Alias for SLOWLOG|STATS|PROPAGATE.
 *
 * The exact propagation behavior depends on the client flags.
 * Specifically:
 *
 * 1. If the client flags CLIENT_FORCE_AOF or CLIENT_FORCE_REPL are set
 *    and assuming the corresponding CMD_CALL_PROPAGATE_AOF/REPL is set
 *    in the call flags, then the command is propagated even if the
 *    dataset was not affected by the command.
 * 2. If the client flags CLIENT_PREVENT_REPL_PROP or CLIENT_PREVENT_AOF_PROP
 *    are set, the propagation into AOF or to slaves is not performed even
 *    if the command modified the dataset.
 *
 * Note that regardless of the client flags, if CMD_CALL_PROPAGATE_AOF
 * or CMD_CALL_PROPAGATE_REPL are not set, then respectively AOF or
 * slaves propagation will never occur.
 *
 * Client flags are modified by the implementation of a given command
 * using the following API:
 *
 * forceCommandPropagation(client *c, int flags);
 * preventCommandPropagation(client *c);
 * preventCommandAOF(client *c);
 * preventCommandReplication(client *c);
 *
 */
func Call(s *Server, c *Client, flags int64) {
	clientOldFlags := c.Flags
	///* Sent the command to clients in MONITOR mode, only if the commands are
	//* not generated from reading an AOF. */
	//if (listLength(server.monitors) &&
	//	!server.loading &&
	//	!(c->cmd->flags & (CMD_SKIP_MONITOR|CMD_ADMIN)))
	//{
	//	replicationFeedMonitors(c,server.monitors,c->db->id,c->argv,c->argc);
	//}
	c.DeleteFlags(CLIENT_FORCE_AOF | CLIENT_FORCE_REPL | CLIENT_PREVENT_PROP)
	//PrevAlsoPropagate := s.AlsoPropagate
	//s.AlsoPropagate.Init()

	dirty := s.Dirty
	start := time.Now()
	c.Cmd.Process(s, c)
	duration := time.Since(start)
	dirty = s.Dirty - dirty
	if dirty < 0 {
		dirty = 0
	}

	if flags&CMD_CALL_PROPAGATE != 0 {
		c.LastCmd.Duration = duration
		c.LastCmd.Calls++
		if c.Flags&CLIENT_PREVENT_AOF_PROP != CLIENT_PREVENT_PROP {
			propagateFlags := PROPAGATE_NONE
			if dirty != 0 {
				propagateFlags |= PROPAGATE_AOF | PROPAGATE_REPL
			}
			if c.WithFlags(CLIENT_FORCE_REPL) {
				propagateFlags |= PROPAGATE_REPL
			}
			if c.WithFlags(CLIENT_FORCE_AOF) {
				propagateFlags |= PROPAGATE_AOF
			}
			if c.WithFlags(CLIENT_PREVENT_REPL_PROP) {
				propagateFlags &= ^PROPAGATE_REPL
			}
			if c.WithFlags(CLIENT_PREVENT_AOF_PROP) {
				propagateFlags &= ^PROPAGATE_AOF
			}
			if propagateFlags != PROPAGATE_NONE && !c.Cmd.WithFlags(CMD_MODULE) {
				s.Propagate(c.Cmd, c.Db.Id, c.Argc, c.Argv, int64(propagateFlags))
			}
		}
	}
	c.DeleteFlags(CLIENT_FORCE_AOF | CLIENT_FORCE_REPL | CLIENT_PREVENT_PROP)
	c.AddFlags(clientOldFlags | CLIENT_FORCE_AOF | CLIENT_FORCE_REPL | CLIENT_PREVENT_PROP)

	//if s.AlsoPropagate.OpNum != 0 {
	//	if flags&CMD_CALL_PROPAGATE != 0 {
	//		for j := int64(0); j < s.AlsoPropagate.OpNum; j++ {
	//			op := s.AlsoPropagate.Ops[j]
	//			target := op.Target
	//			if flags&CMD_CALL_PROPAGATE_AOF == 0 {
	//				target &= ^PROPAGATE_AOF
	//			}
	//			if flags&CMD_CALL_PROPAGATE_REPL == 0 {
	//				target &= ^PROPAGATE_REPL
	//			}
	//			if target != 0 {
	//				s.Propagate(op.Cmd, op.DbId, op.Argc, op.Argv, target)
	//			}
	//		}
	//	}
	//	s.AlsoPropagate.Init()
	//}
	//s.AlsoPropagate = PrevAlsoPropagate
	s.StatNumCommands++
}

/* If this function gets called we already read a whole
 * command, arguments are in the client argv/argc fields.
 * processCommand() execute the command or prepare the
 * server for a bulk read from the client.
 *
 * If C_OK is returned the client is still alive and valid and
 * other operations can be performed by the caller. Otherwise
 * if C_ERR is returned the client was destroyed (i.e. after QUIT). */
func ProcessCommand(s *Server, c *Client) int64 {
	if c.Argv[0] == "quit" {
		/* The QUIT command is handled separately. Normal command procs will
		 * go through checking for replication and QUIT will cause trouble
		 * when FORCE_REPLICATION is enabled and would be implemented in
		 * a regular command proc. */
		s.AddReply(c, s.Shared.Ok)
		c.AddFlags(CLIENT_CLOSE_AFTER_REPLY)
		return C_ERR
	}
	c.Cmd = s.LookUpCommand(c.Argv[0])
	c.LastCmd = c.Cmd
	if c.Cmd == nil {
		//c.FlagTransaction()
		s.AddReplyError(c, fmt.Sprintf("unknown command '%s'", c.Argv[0]))
		return C_OK
	}
	if (c.Cmd.Arity > 0 && c.Cmd.Arity != c.Argc) || c.Argc < -c.Cmd.Arity {
		//c.FlagTransaction()
		s.AddReplyError(c, fmt.Sprintf("wrong number of arguments for '%s' command", c.Argv[0]))
		return C_OK
	}
	if s.RequirePassword != nil && c.Authenticated == 0 && &c.Cmd.Process != &AuthCommand {
		//c.FlagTransaction()
		s.AddReplyError(c, s.Shared.NoAuthErr)
		return C_OK
	}
	//if (server.cluster_enabled &&
	//	!(c->flags & CLIENT_MASTER) &&
	//	!(c->flags & CLIENT_LUA &&
	//		server.lua_caller->flags & CLIENT_MASTER) &&
	//	!(c->cmd->getkeys_proc == NULL && c->cmd->firstkey == 0 &&
	//		c->cmd->proc != execCommand))
	//{
	//	int hashslot;
	//	int error_code;
	//	clusterNode *n = getNodeByQuery(c,c->cmd,c->argv,c->argc,
	//		&hashslot,&error_code);
	//	if (n == NULL || n != server.cluster->myself) {
	//		if (c->cmd->proc == execCommand) {
	//			discardTransaction(c);
	//		} else {
	//			flagTransaction(c);
	//		}
	//		clusterRedirectClient(c,n,hashslot,error_code);
	//		return C_OK;
	//	}
	//}
	/* Handle the maxmemory directive.
	 *
	 * First we try to free some memory if possible (if there are volatile
	 * keys in the dataset). If there are not the only thing we can do
	 * is returning an error. */
	if s.MaxMemory != 0 {
		result := s.FreeMemoryIfNeeded()
		if s.CurrentClient == nil {
			return C_ERR
		}
		if c.Cmd.WithFlags(CMD_DENYOOM) && result == C_ERR {
			//c.FlagTransaction()
			s.AddReplyError(c, s.Shared.OOMErr)
			return C_OK
		}
	}

	///* Don't accept write commands if there are problems persisting on disk
	//* and if this is a master instance. */
	//if (((server.stop_writes_on_bgsave_err &&
	//	server.saveparamslen > 0 &&
	//	server.lastbgsave_status == C_ERR) ||
	//	server.aof_last_write_status == C_ERR) &&
	//	server.masterhost == NULL &&
	//	(c->cmd->flags & CMD_WRITE ||
	//		c->cmd->proc == pingCommand))
	//{
	//	flagTransaction(c);
	//	if (server.aof_last_write_status == C_OK)
	//	addReply(c, shared.bgsaveerr);
	//	else
	//	addReplySds(c,
	//		sdscatprintf(sdsempty(),
	//			"-MISCONF Errors writing to the AOF file: %s\r\n",
	//			strerror(server.aof_last_write_errno)));
	//	return C_OK;
	//}
	///* Don't accept write commands if there are not enough good slaves and
	//* user configured the min-slaves-to-write option. */
	//if (server.masterhost == NULL &&
	//	server.repl_min_slaves_to_write &&
	//	server.repl_min_slaves_max_lag &&
	//	c->cmd->flags & CMD_WRITE &&
	//	server.repl_good_slaves_count < server.repl_min_slaves_to_write)
	//{
	//	flagTransaction(c);
	//	addReply(c, shared.noreplicaserr);
	//	return C_OK;
	//}
	///* Don't accept write commands if this is a read only slave. But
	//* accept write commands if this is our master. */
	//if (server.masterhost && server.repl_slave_ro &&
	//	!(c->flags & CLIENT_MASTER) &&
	//	c->cmd->flags & CMD_WRITE)
	//{
	//	addReply(c, shared.roslaveerr);
	//	return C_OK;
	//}
	///* Only allow SUBSCRIBE and UNSUBSCRIBE in the context of Pub/Sub */
	//if (c->flags & CLIENT_PUBSUB &&
	//	c->cmd->proc != pingCommand &&
	//	c->cmd->proc != subscribeCommand &&
	//	c->cmd->proc != unsubscribeCommand &&
	//	c->cmd->proc != psubscribeCommand &&
	//	c->cmd->proc != punsubscribeCommand) {
	//	addReplyError(c,"only (P)SUBSCRIBE / (P)UNSUBSCRIBE / PING / QUIT allowed in this context");
	//	return C_OK;
	//}
	///* Only allow commands with flag "t", such as INFO, SLAVEOF and so on,
	//* when slave-serve-stale-data is no and we are a slave with a broken
	//* link with master. */
	//if (server.masterhost && server.repl_state != REPL_STATE_CONNECTED &&
	//	server.repl_serve_stale_data == 0 &&
	//	!(c->cmd->flags & CMD_STALE))
	//{
	//	flagTransaction(c);
	//	addReply(c, shared.masterdownerr);
	//	return C_OK;
	//}
	/* Loading DB? Return an error if the command has not the
	 * CMD_LOADING flag. */
	if s.Loading && c.Cmd.WithFlags(CMD_LOADING) {
		s.AddReply(c, s.Shared.LoadingErr)
		return C_OK
	}

	///* Lua script too slow? Only allow a limited number of commands. */
	//if (server.lua_timedout &&
	//	c->cmd->proc != authCommand &&
	//	c->cmd->proc != replconfCommand &&
	//	!(c->cmd->proc == shutdownCommand &&
	//		c->argc == 2 &&
	//		tolower(((char*)c->argv[1]->ptr)[0]) == 'n') &&
	//!(c->cmd->proc == scriptCommand &&
	//	c->argc == 2 &&
	//	tolower(((char*)c->argv[1]->ptr)[0]) == 'k'))
	//{
	//flagTransaction(c);
	//addReply(c, shared.slowscripterr);
	//return C_OK;
	//}
	///* Exec the command */
	//if (c->flags & CLIENT_MULTI &&
	//	c->cmd->proc != execCommand && c->cmd->proc != discardCommand &&
	//	c->cmd->proc != multiCommand && c->cmd->proc != watchCommand)
	//{
	//	queueMultiCommand(c);
	//	addReply(c,shared.queued);
	//} else {
	//call(c,CMD_CALL_FULL);
	//c->woff = server.master_repl_offset;
	//if (listLength(server.ready_keys))
	//handleClientsBlockedOnKeys();
	//}
	// Exec the command
	// multi TODO
	// blocked commands BLPOP TODO
	Call(s, c, CMD_CALL_FULL)
	return C_OK
}

func (s *Server) FreeMemoryIfNeeded() int64 {
	// TODO:
	return C_OK
}

func (s *Server) Propagate(cmd *Command, dbid int64, argc int64, argv []string, flags int64) {
	//TODO:
}

func (s *Server) LookUpCommand(name string) *Command {
	return s.Commands[name]
}

func (s *Server) SetProtocolError(err string, c *Client, pos int64) {
	if s.LogLevel <= LL_INFO {
		//clientStr := c.CatClientInfoString(s)
		errorStr := fmt.Sprintf("Query buffer during protocol error: '%s'", c.QueryBuf)
		buf := make([]byte, len(errorStr))
		for i := 0; i < len(errorStr); i++ {
			if strconv.IsPrint(rune(errorStr[i])) {
				buf[i] = errorStr[i]
			} else {
				buf[i] = '.'
			}
		}
		c.AddFlags(CLIENT_CLOSE_AFTER_REPLY)
		c.QueryBuf = c.QueryBuf[pos:]
	}
}

func (s *Server) GetAllClientInfoString(ctype int64) string {
	str := bytes.Buffer{}
	listIter := s.Clients.ListGetIterator(ITERATION_DIRECTION_INORDER)
	ln := listIter.ListNext()
	for ln != nil {
		c := ln.Value.(*Client)
		if ctype != -1 && c.GetClientType() != ctype {
			continue
		}
		str.WriteString(CatClientInfoString(s, c))
		str.WriteByte('\n')
	}
	return str.String()
}

//func AsyncCloseClientOnOutputBufferLimitReached(s *Server, c *Client) {
//	if c.ReplySize == 0 || c.WithFlags(CLIENT_CLOSE_ASAP) {
//		return
//	}
//	if CheckOutputBufferLimits(s, c) {
//		CloseClientAsync(s, c)
//	}
//}

func DbDeleteSync(s *Server, c *Client, key string) bool {
	// TODO expire things
	c.Db.Delete(key)
	return true
}

func DbDeleteAsync(s *Server, c *Client, key string) bool {
	// TODO
	return true
}

func SelectDB(s *Server, c *Client, dbId int64) int64 {
	if dbId < 0 || dbId >= s.DbNum {
		return C_ERR
	}
	c.Db = s.Dbs[dbId]
	return C_OK
}

func ServerCron(s *Server) {

}

func ClientCron(s *Server, c *Client) {

}

func ServerExists() error {
	if redigoPidFile, err1 := os.Open(os.TempDir() + "//redigo.pid"); err1 == nil {
		defer redigoPidFile.Close()
		if pidStr, err2 := ioutil.ReadAll(redigoPidFile); err2 == nil {
			if pid, err3 := strconv.Atoi(string(pidStr)); err3 == nil {
				if _, err4 := os.FindProcess(pid); err4 == nil {
					return errors.New(fmt.Sprintf("Error! Redigo is runing, Pid is %d", pid))
				}
			}
		}
	}
	return nil
}

func CreateServer() *Server {
	if err := ServerExists(); err == nil {
		pid := os.Getpid()
		redigoPidFile, _ := os.Open(os.TempDir() + "//redigo.pid")
		redigoPidFile.WriteString(fmt.Sprintf("%d", pid))
		redigoPidFile.Close()
		configPath, _ := filepath.Abs(filepath.Dir(os.Args[0]))
		s := Server{
			int64(pid),
			os.TempDir() + "/redigo.pid",
			configPath,
			os.Args[0],
			os.Args,
			10,
			make([]*Db, DEFAULT_DB_NUM),
			DEFAULT_DB_NUM,
			make(map[string]*Command),
			make(map[string]*Command),
			time.Now(),
			0,
			0,
			0,
			9988,
			make([]string, CONFIG_BINDADDR_MAX),
			0,
			os.TempDir() + "/redigo.sock",
			nil,
			nil,
			make(map[int64]*Client),
			nil,
			//make([]ClientBufferLimitsConfig, CLIENT_TYPE_OBUF_COUNT),
			0,
			CONFIG_DEFAULT_MAX_CLIENTS,
			true,
			nil,
			true,
			CONFIG_DEFAULT_PROTO_MAX_BULK_LEN,
			false,
			time.Unix(0, 0),
			0,
			nil,
			0,
			0,
			0,
			0,
			0,
			false,
			sync.RWMutex{},
			CONFIG_DEFAULT_MAXMEMORY,
			false,
			make(chan struct{}, 1),
			LL_DEBUG,
		}
		for i := int64(0); i < s.DbNum; i++ {
			s.Dbs = append(s.Dbs, CreateDb(i))
		}
		s.Clients = ListCreate()
		s.ClientsToClose = ListCreate()
		s.BindAddrs = append(s.BindAddrs, "0.0.0.0")
		s.BindAddrCount++
		return &s
	}
	os.Exit(1)
	return nil
}

func StartServer(s *Server) {
	if s == nil {
		return
	}
	s.ServerLogDebugF("%s\n", "StartServer")
	for _, addr := range s.BindAddrs {
		if addr != "" {
			go TcpServer(s, addr)
		}
	}
	go UnixServer(s)
}

func CloseServer(s *Server) {
	fmt.Println(s.LogLevel)
	s.ServerLogDebugF("%s\n", "CloseServer")
	s.CloseCh <- struct{}{}
	fmt.Println("------>s.CloseCh")
	CloseClientsInAsyncList(s)
	fmt.Println("ListLength---->", s.Clients.ListLength())
	iter := s.Clients.ListGetIterator(ITERATION_DIRECTION_INORDER)
	node := iter.Next
	fmt.Println(node)
	for node != nil {
		CloseClient(s, node.Value.(*Client))
		node = node.ListNextNode()
	}
	fmt.Println("Wait...")
	close(s.CloseCh)
}

func HandleSignal(s *Server) {
	c := make(chan os.Signal, 1)
	fmt.Println("HandleSignal")
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	fmt.Println("Signale Channel", <-c)
	s.ServerLogDebugF("%s\n", "HandleSignal, Pass Block")
	CloseServer(s)
}
