package paxos

//
// Paxos library, to be included in an application.
// Multiple applications will run, each including
// a Paxos peer.
//
// Manages a sequence of agreed-on values.
// The set of peers is fixed.
// Copes with network failures (partition, msg loss, &c).
// Does not store anything persistently, so cannot handle crash+restart.
//
// The application interface:
//
// px = paxos.Make(peers []string, me string)
// px.Start(seq int, v interface{}) -- start agreement on new instance
// px.Status(seq int) (Fate, v interface{}) -- get info about an instance
// px.Done(seq int) -- ok to forget all instances <= seq
// px.Max() int -- highest instance seq known, or -1
// px.Min() int -- instances before this seq have been forgotten
//

import "net"
import "net/rpc"
import "log"

import "os"
import "syscall"
import "sync"
import "sync/atomic"
import "fmt"
import "math/rand"

// px.Status() return values, indicating
// whether an agreement has been decided,
// or Paxos has not yet reached agreement,
// or it was agreed but forgotten (i.e. < Min()).
type Fate int

const (
	Decided   Fate = iota + 1
	Pending        // not yet decided.
	Forgotten      // decided but forgotten.
)

type Paxos struct {
	mu         sync.Mutex
	l          net.Listener
	dead       int32 // for testing
	unreliable int32 // for testing
	rpcCount   int32 // for testing
	peers      []string
	me         int // index into peers[]

	acceptState map[int]AcceptState
	isDecided   map[int]interface{}
	seqMax      int
	doneMax     []int
	deleteSeq   int
	// Your data here.
}
type AcceptState struct {
	prepareMax int
	acceptMax  int
	value      interface{}
}

type Result int

const (
	Ok Result = iota + 1
	Reject
)

type PrepareArgs struct {
	Seq        int
	ProposalID int
}

type PrepareReply struct {
	Reply      Result
	ProposalID int
	Value      interface{}
}

type AccpetArgs struct {
	Seq        int
	ProposalID int
	Value      interface{}
}

type AcceptReply struct {
	Reply Result
	Value interface{}
}

type DecidedArgs struct {
	Seq     int
	Value   interface{}
	From    int
	DoneMax int
}

type DecidedReply struct {
	Reply Result
}

func getAcceptor(accpetState map[int]AcceptState, seq int) AcceptState {
	acceptor, ok := accpetState[seq]
	if !ok {
		acceptor = AcceptState{acceptMax: -1, prepareMax: -1, value: nil}
	}
	return acceptor
}

func getDecidedValue(decidedMap map[int]interface{}, seq int) interface{} {
	decided, ok := decidedMap[seq]
	if !ok {
		decided = nil
	}
	return decided
}

func (px *Paxos) HandlePrepare(args *PrepareArgs, reply *PrepareReply) error {
	px.mu.Lock()
	defer px.mu.Unlock()

	accpetor := getAcceptor(px.acceptState, args.Seq)
	if args.ProposalID > accpetor.prepareMax {
		accpetor.prepareMax = args.ProposalID
		reply.Value = accpetor.value
		reply.ProposalID = accpetor.acceptMax
		reply.Reply = Ok
	} else {
		reply.Reply = Reject
	}
	px.acceptState[args.Seq] = accpetor
	return nil
}

func (px *Paxos) HandleAccpet(args *AccpetArgs, reply *AcceptReply) error {
	px.mu.Lock()
	defer px.mu.Unlock()

	accpetor := getAcceptor(px.acceptState, args.Seq)
	if args.ProposalID >= accpetor.prepareMax {
		accpetor.acceptMax = args.ProposalID
		accpetor.prepareMax = args.ProposalID
		accpetor.value = args.Value

		reply.Reply = Ok
		reply.Value = accpetor.value
	} else {
		reply.Reply = Reject
	}
	px.acceptState[args.Seq] = accpetor
	return nil
}

func (px *Paxos) HandleDecide(args *DecidedArgs, reply *DecidedReply) error {
	px.mu.Lock()
	defer px.mu.Unlock()
	px.isDecided[args.Seq] = args.Value
	px.doneMax[args.From] = args.DoneMax
	return nil
}

//
// call() sends an RPC to the rpcname handler on server srv
// with arguments args, waits for the reply, and leaves the
// reply in reply. the reply argument should be a pointer
// to a reply structure.
//
// the return value is true if the server responded, and false
// if call() was not able to contact the server. in particular,
// the replys contents are only valid if call() returned true.
//
// you should assume that call() will time out and return an
// error after a while if it does not get a reply from the server.
//
// please use call() to send all RPCs, in client.go and server.go.
// please do not change this function.
//
func call(srv string, name string, args interface{}, reply interface{}) bool {
	c, err := rpc.Dial("unix", srv)
	if err != nil {
		err1 := err.(*net.OpError)
		if err1.Err != syscall.ENOENT && err1.Err != syscall.ECONNREFUSED {
			fmt.Printf("paxos Dial() failed: %v\n", err1)
		}
		return false
	}
	defer c.Close()

	err = c.Call(name, args, reply)
	if err == nil {
		return true
	}

	fmt.Println(err)
	return false
}

//
// the application wants paxos to start agreement on
// instance seq, with proposed value v.
// Start() returns right away; the application will
// call Status() to find out if/when agreement
// is reached.
//
func (px *Paxos) Start(seq int, v interface{}) {
	currentMin := px.Min()
	px.mu.Lock()
	defer px.mu.Unlock()
	if seq > px.seqMax {
		px.seqMax = seq
	}
	if seq < currentMin {
		return
	}
	if decided, ok := px.isDecided[seq]; ok && decided != nil {
		return
	}
	go px.Propose(seq, v)
}

func (px *Paxos) PreparePhase(seq int, v interface{}, proposalID int) (bool, interface{}) {
	peerLen := len(px.peers)
	prepareReply := make(chan PrepareReply)
	done := make(chan bool)

	count := 0
	maxProposalID := -1
	var value interface{}
	go func() {
		for {
			reply, more := <-prepareReply
			if !more {
				done <- true
				break
			}
			if reply.Reply == Ok {
				count++
				if reply.ProposalID > maxProposalID {
					maxProposalID = reply.ProposalID
					value = reply.Value
				}
			}
		}
		if value == nil {
			value = v
		}
	}()

	for _, peer := range px.peers {
		args := &PrepareArgs{Seq: seq, ProposalID: proposalID}
		reply := PrepareReply{}
		if peer == px.peers[px.me] {
			px.HandlePrepare(args, &reply)
		} else {
			if ok := call(peer, "Paxos.HandlePrepare", args, &reply); !ok {
				continue
			}
		}
		prepareReply <- reply
	}
	close(prepareReply)
	<-done
	return count >= peerLen/2+1, value
}

func (px *Paxos) AccpetPhase(seq int, proposalID int, value interface{}) bool {
	peerLen := len(px.peers)

	acceptReply := make(chan AcceptReply)
	done := make(chan bool)
	count := 0
	go func() {
		for {
			reply, more := <-acceptReply
			if !more {
				done <- true
				break
			}
			if reply.Reply == Ok {
				count++
			}
		}
	}()
	for _, peer := range px.peers {
		args := &AccpetArgs{Seq: seq, ProposalID: proposalID, Value: value}
		reply := AcceptReply{}
		if peer == px.peers[px.me] {
			px.HandleAccpet(args, &reply)
		} else {
			if ok := call(peer, "Paxos.HandleAccpet", args, &reply); !ok {
				continue
			}
		}
		acceptReply <- reply
	}
	close(acceptReply)
	<-done
	return count >= peerLen/2+1
}

func (px *Paxos) DecidePhase(seq int, value interface{}) {
	for _, peer := range px.peers {
		args := &DecidedArgs{Seq: seq, Value: value, From: px.me, DoneMax: px.doneMax[px.me]}
		reply := DecidedReply{}
		if peer == px.peers[px.me] {
			px.HandleDecide(args, &reply)
		} else {
			if ok := call(peer, "Paxos.HandleDecide", args, &reply); !ok {
				continue
			}
		}
	}
}

func (px *Paxos) Propose(seq int, v interface{}) {

	for {
		peerLen := len(px.peers)
		px.mu.Lock()
		maxSeen := getAcceptor(px.acceptState, seq).prepareMax
		px.mu.Unlock()
		proposalID := (maxSeen+peerLen)/peerLen*peerLen + px.me
		prepareOK, value := px.PreparePhase(seq, v, proposalID)
		if !prepareOK {
			continue
		}
		acceptOK := px.AccpetPhase(seq, proposalID, value)
		if !acceptOK {
			continue
		}
		px.DecidePhase(seq, value)
		break
	}

}

//
// the application on this machine is done with
// all instances <= seq.
//
// see the comments for Min() for more explanation.
//
func (px *Paxos) Done(seq int) {
	px.mu.Lock()
	defer px.mu.Unlock()

	if seq > px.doneMax[px.me] {
		px.doneMax[px.me] = seq
	}
}

//
// the application wants to know the
// highest instance sequence known to
// this peer.
//
func (px *Paxos) Max() int {
	px.mu.Lock()
	defer px.mu.Unlock()
	return px.seqMax
}

//
// Min() should return one more than the minimum among z_i,
// where z_i is the highest number ever passed
// to Done() on peer i. A peers z_i is -1 if it has
// never called Done().
//
// Paxos is required to have forgotten all information
// about any instances it knows that are < Min().
// The point is to free up memory in long-running
// Paxos-based servers.
//
// Paxos peers need to exchange their highest Done()
// arguments in order to implement Min(). These
// exchanges can be piggybacked on ordinary Paxos
// agreement protocol messages, so it is OK if one
// peers Min does not reflect another Peers Done()
// until after the next instance is agreed to.
//
// The fact that Min() is defined as a minimum over
// *all* Paxos peers means that Min() cannot increase until
// all peers have been heard from. So if a peer is dead
// or unreachable, other peers Min()s will not increase
// even if all reachable peers call Done. The reason for
// this is that when the unreachable peer comes back to
// life, it will need to catch up on instances that it
// missed -- the other peers therefor cannot forget these
// instances.
//
func (px *Paxos) Min() int {
	px.mu.Lock()
	defer px.mu.Unlock()
	min := px.doneMax[px.me]
	for _, v := range px.doneMax {
		if v < min {
			min = v
		}
	}
	for ; px.deleteSeq <= min; px.deleteSeq++ {
		delete(px.acceptState, px.deleteSeq)
		delete(px.isDecided, px.deleteSeq)
	}
	return min + 1
}

//
// the application wants to know whether this
// peer thinks an instance has been decided,
// and if so what the agreed value is. Status()
// should just inspect the local peer state;
// it should not contact other Paxos peers.
//
func (px *Paxos) Status(seq int) (Fate, interface{}) {
	px.mu.Lock()
	defer px.mu.Unlock()
	if seq < px.doneMax[px.me] {
		return Forgotten, nil
	}
	if decidedValue := getDecidedValue(px.isDecided, seq); decidedValue != nil {
		return Decided, decidedValue
	}

	return Pending, nil
}

//
// tell the peer to shut itself down.
// for testing.
// please do not change these two functions.
//
func (px *Paxos) Kill() {
	atomic.StoreInt32(&px.dead, 1)
	if px.l != nil {
		px.l.Close()
	}
}

//
// has this peer been asked to shut down?
//
func (px *Paxos) isdead() bool {
	return atomic.LoadInt32(&px.dead) != 0
}

// please do not change these two functions.
func (px *Paxos) setunreliable(what bool) {
	if what {
		atomic.StoreInt32(&px.unreliable, 1)
	} else {
		atomic.StoreInt32(&px.unreliable, 0)
	}
}

func (px *Paxos) isunreliable() bool {
	return atomic.LoadInt32(&px.unreliable) != 0
}

//
// the application wants to create a paxos peer.
// the ports of all the paxos peers (including this one)
// are in peers[]. this servers port is peers[me].
//
func Make(peers []string, me int, rpcs *rpc.Server) *Paxos {
	px := &Paxos{}
	px.peers = peers
	px.me = me

	px.acceptState = make(map[int]AcceptState)
	px.isDecided = make(map[int]interface{})
	npaxos := len(peers)
	px.doneMax = make([]int, npaxos)
	px.deleteSeq = 0
	for i := 0; i < npaxos; i++ {
		px.doneMax[i] = -1
	}
	px.seqMax = -1

	if rpcs != nil {
		// caller will create socket &c
		rpcs.Register(px)
	} else {
		rpcs = rpc.NewServer()
		rpcs.Register(px)

		// prepare to receive connections from clients.
		// change "unix" to "tcp" to use over a network.
		os.Remove(peers[me]) // only needed for "unix"
		l, e := net.Listen("unix", peers[me])
		if e != nil {
			log.Fatal("listen error: ", e)
		}
		px.l = l

		// please do not change any of the following code,
		// or do anything to subvert it.

		// create a thread to accept RPC connections
		go func() {
			for px.isdead() == false {
				conn, err := px.l.Accept()
				if err == nil && px.isdead() == false {
					if px.isunreliable() && (rand.Int63()%1000) < 100 {
						// discard the request.
						conn.Close()
					} else if px.isunreliable() && (rand.Int63()%1000) < 200 {
						// process the request but force discard of reply.
						c1 := conn.(*net.UnixConn)
						f, _ := c1.File()
						err := syscall.Shutdown(int(f.Fd()), syscall.SHUT_WR)
						if err != nil {
							fmt.Printf("shutdown: %v\n", err)
						}
						atomic.AddInt32(&px.rpcCount, 1)
						go rpcs.ServeConn(conn)
					} else {
						atomic.AddInt32(&px.rpcCount, 1)
						go rpcs.ServeConn(conn)
					}
				} else if err == nil {
					conn.Close()
				}
				if err != nil && px.isdead() == false {
					fmt.Printf("Paxos(%v) accept: %v\n", me, err.Error())
				}
			}
		}()
	}

	return px
}
