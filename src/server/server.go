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
	"strings"
	"sync/atomic"
	"net"
	"io"
	"bufio"
)

type accepted struct {
	conn net.Conn
	err  error
}

type Server struct {
	Pid                  int
	PidFile              string
	ConfigFile           string
	ExecFile             string
	ExecArgv             []string
	Hz                   int // serverCron() calls frequency in hertz
	Dbs                  [DEFAULT_DB_NUM]*Db
	DbNum                int
	Commands             map[string]*Command
	OrigCommands         map[string]*Command
	UnixTime             time.Time // UnixTime in nanosecond
	LruClock             time.Time // Clock for LRU eviction
	CronLoopCount        int
	NextClientId         int64
	Port                 int // TCP listening port
	BindAddrs            []string
	BindAddrCount        int       // Number of addresses in test_server.bindaddr[]
	UnixSocketPath       string    // UNIX socket path
	Clients              *SyncList // List of active clients
	ClientsMap           map[int64]*Client
	ClientMaxQueryBufLen int
	ClientMaxReplyBufLen int
	MaxClients           int
	ProtectedMode        bool // Don't accept external connections.
	RequirePassword      *string
	TcpKeepAlive         bool
	ProtoMaxBulkLen      int
	ClientMaxIdleTime    time.Duration
	Dirty                int64 // Changes to DB from the last save
	Shared               *Shared
	StatRejectedConn     int64
	StatConnCount        int64
	StatNetOutputBytes   int64
	StatNetInputBytes    int64
	StatNumCommands      int64
	ConfigFlushAll       bool
	MaxMemory            int
	Loading              bool
	LogLevel             int
	CloseCh              chan struct{}
	mutex                sync.RWMutex
	wg                   sync.WaitGroup
}

//func SetProtocolError(s *Server, c *Client, err string, pos int) {
//	s.ServerLogErrorF("%s\n", err)
//	if s.LogLevel <= LL_INFO {
//		errorStr := fmt.Sprintf("Query buffer during protocol error: '%s'", c.ReadBuf)
//		buf := make([]byte, len(errorStr))
//		for i := 0; i < len(errorStr); i++ {
//			if strconv.IsPrint(rune(errorStr[i])) {
//				buf[i] = errorStr[i]
//			} else {
//				buf[i] = '.'
//			}
//		}
//		c.ReadBuf = c.ReadBuf[pos:]
//	}
//}

func GetAllClientInfoString(s *Server, ctype int) string {
	str := Buffer{}
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

func Call(s *Server, c *Client) {
	// fmt.Println("Call")
	c.Cmd.Process(s, c)
	atomic.AddInt64(&s.StatNumCommands, 1)
}

func ProcessCommand(s *Server, c *Client) int {
	// fmt.Println("ProcessCommand")
	cmdName := strings.ToLower(c.Argv[0])
	// fmt.Println([]byte(cmdName))
	c.Cmd = LookUpCommand(s, cmdName)
	if c.Cmd == nil {
		// fmt.Println("c.Cmd == nil")
		AddReplyError(s, c, fmt.Sprintf("unknown command '%s'", cmdName))
		return C_OK
	}
	if (c.Cmd.Arity > 0 && c.Cmd.Arity != c.Argc) || c.Argc < -c.Cmd.Arity {
		AddReplyError(s, c, fmt.Sprintf("wrong number of arguments for '%s' command", cmdName))
		return C_OK
	}
	if s.RequirePassword != nil && c.Authenticated == 0 && &c.Cmd.Process != &AuthCommand {
		// fmt.Println("Authenticated")
		AddReplyError(s, c, s.Shared.NoAuthErr)
		return C_OK
	}
	Call(s, c)
	return C_OK
}

func LookUpCommand(s *Server, name string) *Command {
	// fmt.Println("LookUpCommand", name)
	return s.Commands[name]
}

func ProcessInlineBuffer(s *Server, c *Client) int {
	// fmt.Println("ProcessInlineBuffer")

	// Search for end of line
	queryBuf := c.ProcessBuf.Bytes()
	size := len(queryBuf)
	newline := IndexOfBytes(queryBuf, 0, size, '\n')
	if newline == -1 {
		if size > s.ClientMaxQueryBufLen {
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

func ProcessMultiBulkBuffer(s *Server, c *Client) int {
	if c.Argc != 0 {
		panic("c.Argc != 0")
	}
	if c.MultiBulkLen == 0 {
		star, err := c.ProcessBuf.ReadByte()
		if err != nil || star != '*' {
			AddReplyError(s, c, fmt.Sprintf("Protocol error: expected '*', got '%c'", star))
			//SetProtocolError(s, c, "expected $ but got something else", 0)
			return C_ERR
		}
		bulkNumStr, err := c.ProcessBuf.ReadStringExclude('\r')
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
		c.ProcessBuf.ReadByte() // pass the \n
		c.MultiBulkLen = bulkNum
		c.Argv = make([]string, c.MultiBulkLen)
		// fmt.Println("c.MultiBulkLen", c.MultiBulkLen)
	}
	if c.MultiBulkLen < 0 {
		return C_ERR
	}
	for c.MultiBulkLen > 0 {
		// Read bulk length if unknown
		dollar, err := c.ProcessBuf.ReadByte()
		if err != nil || dollar != '$' {
			AddReplyError(s, c, fmt.Sprintf("Protocol error: expected '$', got '%c'", dollar))
			//SetProtocolError(s, c, "expected $ but got something else", 0)
			return C_ERR
		}
		bulkLenStr, err := c.ProcessBuf.ReadStringExclude('\r')
		if err != nil {
			AddReplyError(s, c, fmt.Sprintf("Protocol error: invalid bulk length"))
			//SetProtocolError(s, c, "invalid bulk length", 0)
			return C_ERR
		}
		bulkLen, err := strconv.Atoi(bulkLenStr)
		if err != nil || bulkLen > s.ProtoMaxBulkLen {
			AddReplyError(s, c, "Protocol error: invalid bulk length")
			//SetProtocolError(s, c, "invalid bulk length", 0)
			return C_ERR
		}
		c.ProcessBuf.ReadByte() // pass the \n

		bulk := c.ProcessBuf.Next(bulkLen)
		if len(bulk) != bulkLen {
			AddReplyError(s, c, "Protocol error: invalid bulk format")
			//SetProtocolError(s, c, "invalid bulk format", 0)
			return C_ERR
		}
		cr, _ := c.ProcessBuf.ReadByte()
		lf, _ := c.ProcessBuf.ReadByte()
		if cr != '\r' || lf != '\n' {
			AddReplyError(s, c, "Protocol error: invalid bulk format")
			//SetProtocolError(s, c, "invalid bulk format", 0)
			return C_ERR
		}
		c.Argv[len(c.Argv)-c.MultiBulkLen] = string(bulk)
		// fmt.Println("c.MultiBulkLen:", c.MultiBulkLen, ", c.Argv:", c.Argv)
		c.Argc++
		c.MultiBulkLen--
	}
	if c.MultiBulkLen == 0 {
		// fmt.Println("ProcessMultiBulkBuffer", "OK")
		return C_OK
	}
	// fmt.Println("ProcessMultiBulkBuffer", "ERR")
	return C_ERR
}

func ProcessInputBuffer(s *Server, c *Client) {
	// fmt.Println("ProcessInputBuffer")
	c.mutex.Lock()

	if c.RequestType == 0 {
		firstByte, _ := c.ProcessBuf.ReadByteNotGoForward()
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
	c.mutex.Unlock()
}

// Write data in output buffers to client.
func WriteToClient(s *Server, c *Client) {
	c.ReplyWriter.WriteByte(0)
	atomic.AddInt64(&s.StatNetOutputBytes, 1)
	if c.ReplyWriter.Flush() == nil {
		c.SetLastInteraction(s.UnixTime)
	}
}

func ProcessQuery(s *Server, c *Client) {
	ProcessInputBuffer(s, c)
	WriteToClient(s, c)
}

func ReadQuery(s *Server, c *Client, readFinish chan int) {
	// wait write send the signal
	c.QueryCount++
	c.ResetReadStatus()
	
	reader := bufio.NewReaderSize(c.Conn, PROTO_IOBUF_LEN)
	for {
		recieved, err := reader.ReadBytes(0)
		if err == io.EOF { // client side closed connection
			BroadcastCloseClient(c)
			return
		}
		if len(recieved) > 0 {
			c.ReadBuf = append(c.ReadBuf, recieved...)
		}
		if err == nil {
			break
		} else {
			panic("buf full")
		}
	}

	c.SetLastInteraction(s.UnixTime)
	atomic.AddInt64(&s.StatNetInputBytes, int64(len(c.ReadBuf)))
	c.mutex.Lock()
	c.ResetProcessStatus()
	c.ProcessBuf.Write(c.ReadBuf)
	close(readFinish)
	c.mutex.Unlock()
	go ProcessQuery(s, c)

}

func ProcessQueryLoop(s *Server, c *Client) {
	s.wg.Add(1)
	defer s.wg.Done()
	for {
		readFinish := make(chan int, 1)
		go ReadQuery(s, c, readFinish)
		
		select {
		case <-c.CloseCh:
			// server closed, broadcast
			close(readFinish)
			return
		case <-readFinish:
			// query processing finished
		}
	}
}

func CommonServer(s *Server, conn net.Conn, flags int, ip string) {
	c := CreateClient(s, conn, flags)
	if c == nil {
		conn.Close()
		CloseClient(s, c)
	}
	if s.Clients.ListLength() > s.MaxClients {
		AddReplyError(s, c, "max number of clients reached")
		WriteToClient(s, c)
		CloseClient(s, c)
		atomic.AddInt64(&s.StatRejectedConn, 1)
	}
	if s.ProtectedMode && s.BindAddrCount == 0 && s.RequirePassword == nil && flags&CLIENT_UNIX_SOCKET == 0 && ip != "" {
		err := "-DENIED Redis is running in protected mode."
		AddReplyError(s, c, err)
		CloseClient(s, c)
		atomic.AddInt64(&s.StatRejectedConn, 1)
	}
	go ProcessQueryLoop(s, c)
	go CloseClientListener(s, c)
}

func UnixServer(s *Server) {
	// fmt.Println("------>UnixServer")
	s.wg.Add(1)
	defer s.wg.Done()
	listener := AnetListenUnix(s.UnixSocketPath)
	if listener == nil {
		return
	}
	for {
		ch := make(chan accepted, 1)
		go func() {
			conn, err := listener.Accept()
			ch <- accepted{conn, err}
		}()
		select {
		case <-s.CloseCh:
			// s.ServerLogDebugF("-->%v\n", "UnixServer ------ SHUTDOWN")
			listener.Close()
			return
		case acc := <-ch:
			if acc.err != nil {
				// s.ServerLogDebugF("-->%v\n", "UnixServer ------ Accept Error")
				AnetSetErrorFormat("Unix Accept error: %s", acc.err)
				continue
			}
			// s.ServerLogDebugF("-->%v\n", "UnixServer ------ CommonServer")
			CommonServer(s, acc.conn, CLIENT_UNIX_SOCKET, "")
		}
	}
}

func TcpServer(s *Server, ip string) {
	// fmt.Println("------>TcpServer")
	s.wg.Add(1)
	defer s.wg.Done()
	listener := AnetListenTcp("tcp", ip, s.Port)
	defer listener.Close()
	if listener == nil {
		return
	}
	for {
		ch := make(chan accepted, 1)
		go func() {
			conn, err := listener.Accept()
			ch <- accepted{conn, err}
		}()
		select {
		case <-s.CloseCh:
			return
		case acc := <-ch:
			if acc.err != nil {
				continue
			}
			CommonServer(s, acc.conn, 0, ip)
		}
	}
}

func ServerExists() (int, error) {
	// fmt.Println(os.TempDir() + "kiwi.pid")
	if kiwiPidFile, err1 := os.Open(os.TempDir() + "kiwi.pid"); err1 == nil {
		defer kiwiPidFile.Close()
		if pidStr, err2 := ioutil.ReadAll(kiwiPidFile); err2 == nil {
			if pid, err3 := strconv.Atoi(string(pidStr)); err3 == nil {
				if _, err4 := os.FindProcess(pid); err4 == nil {
					return pid, errors.New(fmt.Sprintf("Error! Wiki server is now runing. Pid is %d", pid))
				}
			}
		}
	}
	return 0, nil
}

//func CreateServer() *Server {
//	// fmt.Println("CreateServer")
//	pidFile := os.TempDir() + "kiwi.pid"
//	unixSocketPath := os.TempDir() + "kiwi.sock"
//	if pid, err1 := ServerExists(); err1 == nil {
//		pid = os.Getpid()
//		if kiwiPidFile, err2 := os.Create(pidFile); err2 == nil {
//			kiwiPidFile.WriteString(fmt.Sprintf("%d", pid))
//			kiwiPidFile.Close()
//		}
//
//		configPath, _ := filepath.Abs(filepath.Dir(os.Args[0]))
//		nowTime := time.Now()
//		s := Server{
//			Pid:                  pid,
//			PidFile:              pidFile,
//			ConfigFile:           configPath,
//			ExecFile:             os.Args[0],
//			ExecArgv:             os.Args,
//			Hz:                   10,
//			Dbs:                  make([]*Db, DEFAULT_DB_NUM),
//			DbNum:                DEFAULT_DB_NUM,
//			Commands:             make(map[string]*Command),
//			OrigCommands:         make(map[string]*Command),
//			UnixTime:             nowTime,
//			LruClock:             nowTime,
//			CronLoopCount:        0,
//			NextClientId:         0,
//			Port:                 9988,
//			BindAddrs:            make([]string, CONFIG_BINDADDR_MAX),
//			BindAddrCount:        0,
//			UnixSocketPath:       unixSocketPath,
//			CurrentClient:        nil,
//			Clients:              nil,
//			ClientsMap:           make(map[int]*Client),
//			ClientMaxQueryBufLen: PROTO_INLINE_MAX_SIZE,
//			MaxClients:           CONFIG_DEFAULT_MAX_CLIENTS,
//			ProtectedMode:        true,
//			RequirePassword:      nil,
//			TcpKeepAlive:         true,
//			ProtoMaxBulkLen:      CONFIG_DEFAULT_PROTO_MAX_BULK_LEN,
//			ClientMaxIdleTime:    5 * time.Second,
//			Dirty:                0,
//			Shared:               nil,
//			StatRejectedConn:     0,
//			StatConnCount:        0,
//			StatNetOutputBytes:   0,
//			StatNetInputBytes:    0,
//			StatNumCommands:      0,
//			ConfigFlushAll:       false,
//			MaxMemory:            CONFIG_DEFAULT_MAXMEMORY,
//			Loading:              false,
//			LogLevel:             LL_DEBUG,
//			CloseCh:              make(chan struct{}, 1),
//			mutex:                sync.RWMutex{},
//			wg:                   sync.WaitGroup{},
//		}
//		for i := 0; i < s.DbNum; i++ {
//			s.Dbs = append(s.Dbs, CreateDb(i))
//		}
//		s.Clients = CreateSyncList()
//		s.BindAddrs = append(s.BindAddrs, "0.0.0.0")
//		s.BindAddrCount++
//		// // fmt.Println(&s)
//		PopulateCommandTable(&s)
//		return &s
//	} else {
//		// fmt.Println(err1)
//	}
//	os.Exit(1)
//	return nil
//}

func CreateServer() *Server {
	// fmt.Println("CreateServer")
	pidFile := os.TempDir() + "kiwi.pid"
	unixSocketPath := os.TempDir() + "kiwi.sock"
	pid := os.Getpid()
	fmt.Println("Pid", pid)

	configPath, _ := filepath.Abs(filepath.Dir(os.Args[0]))
	nowTime := time.Now()
	s := Server{
		Pid:                  pid,
		PidFile:              pidFile,
		ConfigFile:           configPath,
		ExecFile:             os.Args[0],
		ExecArgv:             os.Args,
		Hz:                   10,
		Dbs:                  [DEFAULT_DB_NUM]*Db{},
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
	for i := 0; i < s.DbNum; i++ {
		s.Dbs[i] = CreateDb(i)
	}
	s.Clients = CreateSyncList()
	s.BindAddrs = append(s.BindAddrs, "0.0.0.0")
	s.BindAddrCount++
	// // fmt.Println(&s)
	CreateShared(&s)
	PopulateCommandTable(&s)
	return &s

	//if pid, err1 := ServerExists(); err1 == nil {
	//	pid = os.Getpid()
	//	if kiwiPidFile, err2 := os.Create(pidFile); err2 == nil {
	//		kiwiPidFile.WriteString(fmt.Sprintf("%d", pid))
	//		kiwiPidFile.Close()
	//	}
	//
	//} else {
	//	// fmt.Println(err1)
	//}
	os.Exit(1)
	return nil
}

func StartServer(s *Server) {
	// fmt.Println("StartServer")
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
	// fmt.Println("HandleSignal")
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	s.ServerLogDebugF("-->%v: <%v>\n", "Signal", <-c)
	BroadcastCloseServer(s)
	s.wg.Wait()
	os.Exit(0)
}

func CloseServerListener(s *Server) {
	// fmt.Println("CloseServerListener")
	s.wg.Add(1)
	defer s.wg.Done()
	select {
	case <-s.CloseCh:
		// fmt.Println("CloseServerListener ----> Close Server")
		CloseServer(s)
	}
}

func CloseServer(s *Server) {
	// fmt.Println("CloseServer")
	// clear clients
	iter := s.Clients.ListGetIterator(ITERATION_DIRECTION_INORDER)
	for node := iter.ListNext(); node != nil; node = iter.ListNext() {
		BroadcastCloseClient(node.Value.(*Client))
	}
	defer os.Remove(s.UnixSocketPath)
	defer os.Remove(s.PidFile)
}

func BroadcastCloseServer(s *Server) {
	// fmt.Println("BroadcastCloseServer")
	close(s.CloseCh)
}
