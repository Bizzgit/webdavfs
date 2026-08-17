
package main

import (
	"context"
	"os"
	"runtime/debug"
	"runtime/pprof"
	"strings"
	"sync"
	"syscall"
	"time"
	"bazil.org/fuse"
	"bazil.org/fuse/fs"
)

const (
	RefIO = iota
	RefMeta
)

type Node struct {
	Dnode
	Atime		time.Time
	LastStat	time.Time
	DirCache	map[string]Dnode
	DirCacheTime	time.Time
	Inode		uint64
	RefCount	[2]int
	Deleted		bool
	Parent		*Node
	Child		map[string]*Node
	InUse		bool
}

var rootNode = &Node{
	Inode:		1,
	Child:		make(map[string]*Node),
}

var EBUSY = fuse.Errno(syscall.EBUSY)
var EINTR = fuse.Errno(syscall.EINTR)
var nodeMutex sync.Mutex
// nodeCond signals waiters (incIoRef/incMetaRef) whenever a ref count
// changes, replacing the old sleep-and-poll busy loop.
var nodeCond = sync.NewCond(&nodeMutex)
var lockRef = 0
var lockTimer *time.Timer

func lockWatchdogStart(name string) {
	if trace(T_LOCK) {
		stack := debug.Stack()
		lockTimer = time.AfterFunc(2 * time.Second, func() {
			tPrintf("LOCKERR (%s) Lock held longer than 2 seconds:\n%s",
				name, stack)
			tPrintf("== dump of all goroutines:")
			pprof.Lookup("goroutine").WriteTo(os.Stdout, 1)
		})
	}
	lockRef++
}

func lockWatchdogStop() {
	if trace(T_LOCK) {
		if lockRef != 1 {
			tPrintf("LOCKERR unlock: lockRef %d != 1\n%s",
				lockRef, debug.Stack())
		}
		if lockTimer == nil {
			tPrintf("LOCKERR unlock: lockTimer == nil\n%s",
				debug.Stack())
		} else {
			lockTimer.Stop()
			lockTimer = nil
		}
	}
	lockRef--
}

func (nd *Node) Lock() {
	nodeMutex.Lock()
	lockWatchdogStart(nd.Name)
	// dbgPrintf("node: Lock %s @ %p ref %d\n", nd.Name, nd, lockRef)
}

func (nd *Node) Unlock() {
	lockWatchdogStop()
	// dbgPrintf("node: Unlock %s @ %p ref %d\n", nd.Name, nd, lockRef)
	nodeMutex.Unlock()
}

// waitCond calls nodeCond.Wait(), which unlocks/relocks nodeMutex
// directly and so bypasses Node.Lock()/Unlock(). To keep the T_LOCK
// watchdog (lockRef/lockTimer) consistent with the mutex actually being
// released during the wait, "release" that bookkeeping before Wait()
// and "reacquire" it after - otherwise ordinary contention produces
// false LOCKERR spam and leaks an unstopped 2s timer that fires (and
// dumps every goroutine) on any wait over 2 seconds, which stops being
// a rare diagnostic signal and starts being the normal case.
func (nd *Node) waitCond() {
	lockWatchdogStop()
	nodeCond.Wait()
	lockWatchdogStart(nd.Name)
}

// ctxWatch spawns a goroutine that calls nodeCond.Broadcast() once ctx
// is done, so a goroutine blocked in nodeCond.Wait() - which has no
// native way to observe context cancellation - gets woken up promptly
// instead of only when some unrelated node's ref count happens to
// change. The caller must call the returned stop function once it's
// done waiting, so the goroutine can exit.
func ctxWatch(ctx context.Context) (stop func()) {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			nodeCond.Broadcast()
		case <-done:
		}
	}()
	return func() { close(done) }
}

func (nd *Node) addNode(d Dnode, really bool) *Node {
	n := nd.Child[d.Name]
	if n != nil {
		if really {
			n.InUse = true
		}
		n.LastStat = time.Now()
		n.Dnode = d
		return n
	}
	nn := &Node {
		Inode: fs.GenerateDynamicInode(nd.Inode, d.Name),
		Dnode: d,
		Parent: nd,
		InUse: really,
		LastStat: time.Now(),
	}
	if d.IsDir {
		nn.Child = map[string]*Node{}
	}
	nd.Child[d.Name] = nn
	// dbgPrintf("node: addNode %s @ %p to %s @ %p\n", nn.Name, nn, nd.Name, nd)
	return nn
}

func (nd *Node) delNode(name string) {
	n := nd.getNode(name)
	if n != nil {
		// dbgPrintf("node: delNode %s @ %p from %s @ %p\n", n.Name, n, nd.Name, nd)
		n.Name = n.Name + " (deleted)"
		n.Deleted = true
		delete(nd.Child, name)
	} else {
		// dbgPrintf("node: delNode %s from %s @ %p: not found\n", name, nd.Name, nd)
	}
}

func (nd *Node) forgetNode() {
	if nd.Parent != nil {
		// dbgPrintf("node: forgetNode %s @ %p from %s %p\n", nd.Name, nd, nd.Parent.Name, nd.Parent)
		// paranoia - check should always succeed.
		parent := nd.Parent
		if parent != nil && parent.Child[nd.Name] == nd {
			delete(nd.Parent.Child, nd.Name)
		}
	} else {
		// dbgPrintf("node: forgetNode %s @ %p (no parent)\n", nd.Name, nd)
	}
}

func (nd *Node) moveNode(dest *Node, oldName string, newName string) {
	dest.delNode(newName)
	cn := nd.getNode(oldName)
	// dbgPrintf("node: moveNode %s@%p/%s@%p -> %s@%p/%s\n", nd.getPath(), nd, oldName, cn, dest.getPath(), dest, newName)
	if cn != nil {
		delete(nd.Child, oldName)
		cn.Name = newName
		cn.Parent = dest
		dest.Child[newName] = cn
	}
}

func (nd *Node) getNode(name string) *Node {
	if nd.Child != nil {
		return nd.Child[name]
	}
	return nil
}

func (nd *Node) deleteUnusedChildren() {
	for name, nn := range nd.Child {
		nn.deleteUnusedChildren()
		if !nn.InUse && len(nn.Child) == 0 {
			delete(nd.Child, name)
		}
	}
}

func (nd *Node) invalidateThisNode() {
	nd.deleteUnusedChildren()
	if !nd.InUse && len(nd.Child) == 0 {
		nd.forgetNode()
	}
}

func (nd *Node) invalidateNode(name string) {
	nn := nd.Child[name]
	if nn != nil {
		nn.invalidateThisNode()
	}
}

func lookupNode(path string) (de *Node) {
	d := rootNode
	if path != "/" {
		pelem := strings.Split(path[1:], "/")
		for _, n := range(pelem) {
			if d.Child == nil || d.Child[n] == nil {
				return
			}
			d = d.Child[n]
		}
	}
	de = d
	return
}

func (de *Node) getPath() string {
	if de.Parent == nil {
		return "/"
	}
	a := make([]string, 0, 16)
	for d := de; d != nil && d.Parent != nil; d = d.Parent {
		a = append(a, d.Name)
	}
	// reverse
	for i, j := 0, len(a) - 1; i < j; i, j = i + 1, j - 1 {
		a[i], a[j] = a[j], a[i]
	}
	path := "/" + strings.Join(a, "/")
	// dbgPrintf("node: getPath %p -> %s\n", de, path)
	return path
}

// no IO operations must be going on at this node or above.
// perhaps use a global refcount as well - faster
func (de *Node) doesIO() bool {
	if de.RefCount[RefIO] > 0 {
		return true
	}
	for _, d := range de.Child {
		if d.doesIO() {
			return true
		}
	}
	return false
}

// no meta operations can be going on at this node or below.
// perhaps use a global refcount as well - faster
func (de *Node) doesMeta() bool {
	for d := de; d != nil; d = d.Parent {
		if d.RefCount[RefMeta] > 0 {
			return true
		}
	}
	return false
}

// incIoRef blocks until no meta operation is in progress on this node
// or any ancestor, then registers an I/O op. Returns EINTR if ctx is
// cancelled before that - the caller must not treat the ref as held in
// that case (nothing to undo: RefCount is only bumped on success).
func (de *Node) incIoRef(ctx context.Context, id fuse.RequestID) (err error) {
	de.Lock()
	defer de.Unlock()
	stop := ctxWatch(ctx)
	defer stop()
	for de.doesMeta() {
		if ctx.Err() != nil {
			return EINTR
		}
		de.waitCond()
	}
	if ctx.Err() != nil {
		return EINTR
	}
	de.RefCount[RefIO]++
	return
}

func (de *Node) decIoRef() {
	de.Lock()
	de.RefCount[RefIO]--
	nodeCond.Broadcast()
	de.Unlock()
}

// Waits for other meta ops to clear, registers this one, then waits for
// i/o to cease before returning. Caller must already hold de.Lock()
// (see incMetaRefThenLock). On cancellation, any partially-acquired
// state (the meta ref, if already bumped) is rolled back before
// returning EINTR, so a cancelled caller never leaks a held ref.
func (de *Node) incMetaRef(ctx context.Context, id fuse.RequestID) error {
	stop := ctxWatch(ctx)
	defer stop()
	// first wait for other meta operations
	for de.doesMeta() {
		if ctx.Err() != nil {
			return EINTR
		}
		de.waitCond()
	}
	if ctx.Err() != nil {
		return EINTR
	}
	de.RefCount[RefMeta]++
	// now wait for i/o operations to cease.
	for de.doesIO() {
		if ctx.Err() != nil {
			de.RefCount[RefMeta]--
			nodeCond.Broadcast()
			return EINTR
		}
		de.waitCond()
	}
	// dbgPrintf("node: incMetaRef %s@%p: ref now %d\n", de.Name, de, de.RefCount[RefMeta])
	return nil
}

// incMetaRefThenLock always returns with de.Lock() held, regardless of
// err - callers must Unlock() in both the error and success path (see
// call sites in fuse.go).
func (de *Node) incMetaRefThenLock(ctx context.Context, id fuse.RequestID) (err error) {
	de.Lock()
	err = de.incMetaRef(ctx, id)
	return
}

func (de *Node) decMetaRef() {
	de.RefCount[RefMeta]--
	nodeCond.Broadcast()
	// dbgPrintf("node: decMetaRef %s@%p: ref now %d\n", de.Name, de, de.RefCount[RefMeta])
}

