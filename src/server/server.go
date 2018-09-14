package server

import (
	"sync"
	"fmt"
	"strconv"
	"time"
	"os"
	"os/signal"
	"syscall"
	"io/ioutil"
	"errors"
	"path/filepath"
	"io"
	"bytes"
	"bufio"
	"strings"
)

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
	UnixTime             time.Time // UnixTime in nanosecond
	LruClock             time.Time // Clock for LRU eviction
	CronLoopCount        int64
	NextClientId         int64
	Port                 int64 // TCP listening port
	BindAddrs            []string
	BindAddrCount        int64  // Number of addresses in test_server.bindaddr[]
	UnixSocketPath       string // UNIX socket path
	CurrentClient        *Client
	Clients              *SyncList // List of active clients
	ClientsMap           map[int64]*Client
	ClientMaxQueryBufLen int64
	ClientMaxReplyBufLen int64
	MaxClients           int64
	ProtectedMode        bool // Don't accept external connections.
	RequirePassword      *string
	TcpKeepAlive         bool
	ProtoMaxBulkLen      int64
	ClientMaxIdleTime    time.Duration
	Dirty                int64 // Changes to DB from the last save
	Shared               *SharedObjects
	StatRejectedConn     int64
	StatConnCount        int64
	StatNetOutputBytes   int64
	StatNetInputBytes    int64
	StatNumCommands      int64
	ConfigFlushAll       bool
	MaxMemory            int64
	Loading              bool
	LogLevel             int64
	CloseCh              chan struct{}
	mutex                sync.RWMutex
	wg                   sync.WaitGroup
}

func LruClock(s *Server) time.Time {
	if 1000/s.Hz <= LRU_CLOCK_RESOLUTION {
		return s.LruClock
	} else {
		return GetLruClock()
	}
}

func GetLruClock() time.Time {
	return time.Now()
}

func LinkClient(s *Server, c *Client) {
	s.Clients.ListAddNodeTail(c)
	s.ClientsMap[c.Id] = c
	c.Node = s.Clients.ListTail()
	s.StatConnCount++
}

func UnLinkClient(s *Server, c *Client) {
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
}

func CloseClient(s *Server, c *Client) {
	fmt.Println("CloseClient")
	c.QueryBuf.Reset()
	c.ReplyWriter = nil
	c.ResetArgv()
	c.QueryBuf = nil
	c.ReplyWriter = nil
	UnLinkClient(s, c)
}

func GetClientById(s *Server, id int64) *Client {
	return s.ClientsMap[id]
}

// Write data in output buffers to client.
func WriteToClient(s *Server, c *Client) {
	c.ReplyWriter.WriteByte(0)
	s.mutex.Lock()
	s.StatNetOutputBytes++
	s.mutex.Unlock()
	err := c.ReplyWriter.Flush()
	if err != nil {
		return
	}
	c.SetLastInteraction(s.UnixTime)
	c.Reset()
}

func ProcessInlineBuffer(s *Server, c *Client) int64 {
	// Search for end of line
	queryBuf := c.QueryBuf.Bytes()
	size := len(queryBuf)
	newline := IndexOfBytes(queryBuf, 0, size, '\n')
	if newline == -1 {
		if int64(size) > s.ClientMaxQueryBufLen {
			AddReplyError(s, c, "Protocol error: too big inline request")
			//SetProtocolError(s, c, "too big inline request", 0)
		}
		return C_ERR
	}
	if newline != 0 && newline != size && queryBuf[newline-1] == '\r' {
		// Handle the \r\n case.
		newline--
	}
	/* Split the input buffer up to the \r\n */
	argvs := SplitArgs(queryBuf[0:newline])
	if argvs == nil {
		AddReplyError(s, c, "Protocol error: unbalanced quotes in request")
		//SetProtocolError(s, c, "unbalanced quotes in inline request", 0)
		return C_ERR
	}

	// Leave data after the first line of the query in the buffer
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

func ProcessMultiBulkBuffer(s *Server, c *Client) int64 {
	if c.Argc != 0 {
		panic("c.Argc != 0")
	}
	if c.MultiBulkLen == 0 {
		star, err := c.QueryBuf.ReadByte()
		if err != nil || star != '*' {
			AddReplyError(s, c, fmt.Sprintf("Protocol error: expected '*', got '%c'", star))
			//SetProtocolError(s, c, "expected $ but got something else", 0)
			return C_ERR
		}
		bulkNumStr, err := c.QueryBuf.ReadStringExclude('\r')
		if err != nil {
			return C_ERR
		}

		bulkNum, err := strconv.Atoi(bulkNumStr)
		if err != nil || bulkNum > 1024*1024 {
			AddReplyError(s, c, "Protocol error: invalid multibulk length")
			//SetProtocolError(s, c, "invalid multibulk length", 0)
			return C_ERR
		}
		if bulkNum <= 0 {
			return C_OK
		}
		c.QueryBuf.ReadByte() // pass the \n
		c.MultiBulkLen = int64(bulkNum)
		c.Argv = make([]string, c.MultiBulkLen)
	}
	if c.MultiBulkLen < 0 {
		return C_ERR
	}
	for c.MultiBulkLen > 0 {
		// Read bulk length if unknown
		dollar, err := c.QueryBuf.ReadByte()
		if err != nil || dollar != '$' {
			AddReplyError(s, c, fmt.Sprintf("Protocol error: expected '$', got '%c'", dollar))
			//SetProtocolError(s, c, "expected $ but got something else", 0)
			return C_ERR
		}
		bulkLenStr, err := c.QueryBuf.ReadStringExclude('\r')
		if err != nil {
			AddReplyError(s, c, fmt.Sprintf("Protocol error: invalid bulk length"))
			//SetProtocolError(s, c, "invalid bulk length", 0)
			return C_ERR
		}
		bulkLen, err := strconv.Atoi(bulkLenStr)
		if err != nil || int64(bulkLen) > s.ProtoMaxBulkLen {
			AddReplyError(s, c, "Protocol error: invalid bulk length")
			//SetProtocolError(s, c, "invalid bulk length", 0)
			return C_ERR
		}
		c.QueryBuf.ReadByte() // pass the \n

		bulk := c.QueryBuf.Next(bulkLen)
		if len(bulk) != bulkLen {
			AddReplyError(s, c, "Protocol error: invalid bulk format")
			//SetProtocolError(s, c, "invalid bulk format", 0)
			return C_ERR
		}
		cr, _ := c.QueryBuf.ReadByte()
		lf, _ := c.QueryBuf.ReadByte()
		if cr != '\r' || lf != '\n' {
			AddReplyError(s, c, "Protocol error: invalid bulk format")
			//SetProtocolError(s, c, "invalid bulk format", 0)
			return C_ERR
		}
		c.Argv = append(c.Argv, string(bulk))
		c.Argc++
		c.MultiBulkLen--
	}
	if c.MultiBulkLen == 0 {
		return C_OK
	}
	return C_ERR
}

func ProcessInputBuffer(s *Server, c *Client) {
	s.mutex.Lock()
	s.CurrentClient = c
	if c.RequestType == 0 {
		firstByte, _ := c.QueryBuf.ReadByteNotGoForward()
		if firstByte == '*' {
			c.RequestType = PROTO_REQ_MULTIBULK
		} else {
			c.RequestType = PROTO_REQ_INLINE
		}
	}
	if c.RequestType == PROTO_REQ_INLINE {
		if ProcessInlineBuffer(s, c) != C_OK {
		}
	} else if c.RequestType == PROTO_REQ_MULTIBULK {
		if ProcessMultiBulkBuffer(s, c) != C_OK {
		}
	} else {
		panic("Unknown request type")
	}

	if c.Argc != 0 {
		ProcessCommand(s, c)
	}
	s.CurrentClient = nil
	s.mutex.Unlock()
}

func ReadFromClient(s *Server, c *Client, readCh chan int64) {
	reader := bufio.NewReaderSize(c.Conn, PROTO_IOBUF_LEN)
	for {
		recieved, err := reader.ReadSlice(0)
		fmt.Println("recieved----->", len(recieved))
		if err != nil {
			fmt.Println(err)
			if err == io.EOF {
				readCh <- C_ERR
				BroadcastCloseClient(c)
				return
			}
		}
		if len(recieved) > 0 {
			c.QueryBuf.Write(recieved)
		}
	}
	c.ReadCount++
	if !c.WithFlags(CLIENT_LUA) && c.MaxIdleTime == 0 {
		c.HeartBeatCh <- c.ReadCount
	}
	c.SetLastInteraction(s.UnixTime)
	s.mutex.Lock()
	s.StatNetInputBytes += int64(c.QueryBuf.Len())
	s.mutex.Unlock()
	ProcessInputBuffer(s, c)
	readCh <- C_OK
}

func Call(s *Server, c *Client) {
	c.Cmd.Process(s, c)
	s.mutex.Lock()
	s.StatNumCommands++
	s.mutex.Unlock()
}

func ProcessCommand(s *Server, c *Client) int64 {
	cmdName := strings.ToLower(c.Argv[0])
	c.Cmd = LookUpCommand(s, cmdName)
	if c.Cmd == nil {
		AddReplyError(s, c, fmt.Sprintf("unknown command '%s'", cmdName))
		return C_OK
	}
	if (c.Cmd.Arity > 0 && c.Cmd.Arity != c.Argc) || c.Argc < -c.Cmd.Arity {
		AddReplyError(s, c, fmt.Sprintf("wrong number of arguments for '%s' command", cmdName))
		return C_OK
	}
	if s.RequirePassword != nil && c.Authenticated == 0 && &c.Cmd.Process != &AuthCommand {
		AddReplyError(s, c, s.Shared.NoAuthErr)
		return C_OK
	}
	Call(s, c)
	return C_OK
}

func LookUpCommand(s *Server, name string) *Command {
	return s.Commands[name]
}

//func SetProtocolError(s *Server, c *Client, err string, pos int64) {
//	s.ServerLogErrorF("%s\n", err)
//	if s.LogLevel <= LL_INFO {
//		errorStr := fmt.Sprintf("Query buffer during protocol error: '%s'", c.QueryBuf)
//		buf := make([]byte, len(errorStr))
//		for i := 0; i < len(errorStr); i++ {
//			if strconv.IsPrint(rune(errorStr[i])) {
//				buf[i] = errorStr[i]
//			} else {
//				buf[i] = '.'
//			}
//		}
//		c.QueryBuf = c.QueryBuf[pos:]
//	}
//}

func GetAllClientInfoString(s *Server, ctype int64) string {
	str := bytes.Buffer{}
	iter := s.Clients.ListGetIterator(ITERATION_DIRECTION_INORDER)
	for node := iter.ListNext(); node != nil; node = iter.ListNext() {
		c := node.Value.(*Client)
		if ctype != -1 && c.GetClientType() != ctype {
			continue
		}
		str.WriteString(CatClientInfoString(s, c))
		str.WriteByte('\n')
	}
	return str.String()
}

func DbDeleteSync(s *Server, c *Client, key string) bool {
	// TODO expire things
	c.Db.Delete(key)
	return true
}

func DbDeleteAsync(s *Server, c *Client, key string) bool {
	// TODO
	c.Db.Delete(key)
	return true
}

func SelectDB(s *Server, c *Client, dbId int64) int64 {
	if dbId < 0 || dbId >= s.DbNum {
		return C_ERR
	}
	c.Db = s.Dbs[dbId]
	return C_OK
}

func UpdateCachedTime(s *Server) {
	s.UnixTime = time.Now()
}

func UpdateLRUClock(s *Server) {
	s.LruClock = time.Now()
}

func ServerCronHandler(s *Server) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.wg.Add(1)
	defer s.wg.Done()
	UpdateCachedTime(s)
	UpdateLRUClock(s)
	s.CronLoopCount++
}

func ServerCron(s *Server) {
	s.wg.Add(1)
	defer s.wg.Done()
	for {
		select {
		case <-s.CloseCh:
			s.ServerLogDebugF("-->%v\n", "ServerCron ------ SHUTDOWN")
			return
		case <-time.After(time.Millisecond * time.Duration(1000/s.Hz)):
			go ServerCronHandler(s)
		}
	}
}

func ServerExists() (int, error) {
	fmt.Printf("-->%v\n", "ServerExists")
	if redigoPidFile, err1 := os.Open(os.TempDir() + "KiwiDB.pid"); err1 == nil {
		defer redigoPidFile.Close()
		if pidStr, err2 := ioutil.ReadAll(redigoPidFile); err2 == nil {
			if pid, err3 := strconv.Atoi(string(pidStr)); err3 == nil {
				if _, err4 := os.FindProcess(pid); err4 == nil {
					return pid, errors.New(fmt.Sprintf("Error! Redigo test_server is now runing. Pid is %d", pid))
				}
			}
		}
	}
	return 0, nil
}

func CreateServer() *Server {
	fmt.Println("CreateServer")
	pidFile := os.TempDir() + "KiwiDB.pid"
	unixSocketPath := os.TempDir() + "KiwiDB.sock"
	if pid, err1 := ServerExists(); err1 == nil {
		pid = os.Getpid()
		if redigoPidFile, err2 := os.Create(pidFile); err2 == nil {
			redigoPidFile.WriteString(fmt.Sprintf("%d", pid))
			redigoPidFile.Close()
		}

		configPath, _ := filepath.Abs(filepath.Dir(os.Args[0]))
		nowTime := time.Now()
		s := Server{
			Pid:                  int64(pid),
			PidFile:              pidFile,
			ConfigFile:           configPath,
			ExecFile:             os.Args[0],
			ExecArgv:             os.Args,
			Hz:                   10,
			Dbs:                  make([]*Db, DEFAULT_DB_NUM),
			DbNum:                DEFAULT_DB_NUM,
			Commands:             make(map[string]*Command),
			OrigCommands:         make(map[string]*Command),
			UnixTime:             nowTime,
			LruClock:             nowTime,
			CronLoopCount:        0,
			NextClientId:         0,
			Port:                 9988,
			BindAddrs:            make([]string, CONFIG_BINDADDR_MAX),
			BindAddrCount:        0,
			UnixSocketPath:       unixSocketPath,
			CurrentClient:        nil,
			Clients:              nil,
			ClientsMap:           make(map[int64]*Client),
			ClientMaxQueryBufLen: PROTO_INLINE_MAX_SIZE,
			MaxClients:           CONFIG_DEFAULT_MAX_CLIENTS,
			ProtectedMode:        true,
			RequirePassword:      nil,
			TcpKeepAlive:         true,
			ProtoMaxBulkLen:      CONFIG_DEFAULT_PROTO_MAX_BULK_LEN,
			ClientMaxIdleTime:    5 * time.Second,
			Dirty:                0,
			Shared:               nil,
			StatRejectedConn:     0,
			StatConnCount:        0,
			StatNetOutputBytes:   0,
			StatNetInputBytes:    0,
			StatNumCommands:      0,
			ConfigFlushAll:       false,
			MaxMemory:            CONFIG_DEFAULT_MAXMEMORY,
			Loading:              false,
			LogLevel:             LL_DEBUG,
			CloseCh:              make(chan struct{}, 1),
			mutex:                sync.RWMutex{},
			wg:                   sync.WaitGroup{},
		}
		for i := int64(0); i < s.DbNum; i++ {
			s.Dbs = append(s.Dbs, CreateDb(i))
		}
		s.Clients = CreateSyncList()
		s.BindAddrs = append(s.BindAddrs, "0.0.0.0")
		s.BindAddrCount++
		CreateShared(&s)
		return &s
	} else {
		fmt.Println(err1)
	}
	os.Exit(1)
	return nil
}

func StartServer(s *Server) {
	fmt.Println("StartServer")
	if s == nil {
		return
	}
	for _, addr := range s.BindAddrs {
		if addr != "" {
			go TcpServer(s, addr)
		}
	}
	//go UnixServer(s)
	go ServerCron(s)
	go CloseServerListener(s)
}

func HandleSignal(s *Server) {
	fmt.Println("HandleSignal")
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	s.ServerLogDebugF("-->%v: <%v>\n", "Signal", <-c)
	BroadcastCloseServer(s)
	s.wg.Wait()
	os.Exit(0)
}

func CloseServerListener(s *Server) {
	s.wg.Add(1)
	defer s.wg.Done()
	select {
	case <-s.CloseCh:
		fmt.Println("CloseServerListener ----> Close Server")
		CloseServer(s)
	}
}

func CloseServer(s *Server) {
	fmt.Println("CloseServer")
	// clear clients
	iter := s.Clients.ListGetIterator(ITERATION_DIRECTION_INORDER)
	for node := iter.ListNext(); node != nil; node = iter.ListNext() {
		BroadcastCloseClient(node.Value.(*Client))
	}
	defer os.Remove(s.UnixSocketPath)
	defer os.Remove(s.PidFile)
}
