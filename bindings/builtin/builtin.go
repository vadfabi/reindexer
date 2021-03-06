package builtin

// #include "core/cbinding/reindexer_c.h"
// #include "reindexer_cgo.h"
// #include <stdlib.h>
import "C"
import (
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"runtime"
	"sync"
	"unsafe"

	"github.com/restream/reindexer/bindings"
	"github.com/restream/reindexer/cjson"
)

const defCgoLimit = 2000

var bufFree = newBufFreeBatcher()

// Logger interface for reindexer
type Logger interface {
	Printf(level int, fmt string, msg ...interface{})
}

var logger Logger
var logMtx sync.Mutex

var bufPool sync.Pool

type Builtin struct {
	cgoLimiter chan struct{}
	rx         C.uintptr_t
}

type RawCBuffer struct {
	cbuf         C.reindexer_resbuffer
	hasFinalizer bool
}

func (buf *RawCBuffer) FreeFinalized() {
	buf.hasFinalizer = false
	if buf.cbuf.results_ptr != 0 {
		CGoLogger(bindings.WARNING, "FreeFinalized called. Iterator.Close() was not called")
	}
	buf.Free()
}

func (buf *RawCBuffer) Free() {
	bufFree.add(buf)
}

func (buf *RawCBuffer) GetBuf() []byte {
	return buf2go(buf.cbuf)
}

func newRawCBuffer() *RawCBuffer {
	obj := bufPool.Get()
	if obj != nil {
		return obj.(*RawCBuffer)
	}
	return &RawCBuffer{}
}

func init() {
	bindings.RegisterBinding("builtin", &Builtin{})
}

func str2c(str string) C.reindexer_string {
	hdr := (*reflect.StringHeader)(unsafe.Pointer(&str))
	return C.reindexer_string{p: unsafe.Pointer(hdr.Data), n: C.int(hdr.Len)}
}

func err2go(ret C.reindexer_error) error {
	if ret.what != nil {
		defer C.free(unsafe.Pointer(ret.what))
		return bindings.NewError("rq:"+C.GoString(ret.what), int(ret.code))
	}
	return nil
}

func ret2go(ret C.reindexer_ret) (*RawCBuffer, error) {
	if ret.err_code != 0 {
		defer C.free(unsafe.Pointer(uintptr(ret.out.data)))
		return nil, bindings.NewError("rq:"+C.GoString((*C.char)(unsafe.Pointer(uintptr(ret.out.data)))), int(ret.err_code))
	}

	rbuf := newRawCBuffer()
	rbuf.cbuf = ret.out
	if !rbuf.hasFinalizer {
		runtime.SetFinalizer(rbuf, (*RawCBuffer).FreeFinalized)
		rbuf.hasFinalizer = true
	}
	return rbuf, nil
}

func buf2c(buf []byte) C.reindexer_buffer {
	if len(buf) == 0 {
		return C.reindexer_buffer{data: nil, len: 0}
	}
	return C.reindexer_buffer{
		data: (*C.uint8_t)(unsafe.Pointer(&buf[0])),
		len:  C.int(len(buf)),
	}
}

func buf2go(buf C.reindexer_resbuffer) []byte {
	if buf.data == 0 || buf.len == 0 {
		return nil
	}
	length := int(buf.len)
	return (*[1 << 30]byte)(unsafe.Pointer(uintptr(buf.data)))[:length:length]
}

func bool2cint(v bool) C.int {
	if v {
		return 1
	}
	return 0
}

func (binding *Builtin) Init(u *url.URL, options ...interface{}) error {
	if binding.rx != 0 {
		return bindings.NewError("already initialized", bindings.ErrConflict)
	}

	cgoLimit := defCgoLimit
	var rx uintptr = 0
	for _, option := range options {
		switch v := option.(type) {
		case bindings.OptionCgoLimit:
			cgoLimit = v.CgoLimit
		case bindings.OptionReindexerInstance:
			rx = v.Instance
		default:
			fmt.Printf("Unknown builtin option: %v\n", option)
		}
	}

	if rx == 0 {
		binding.rx = C.init_reindexer()
	} else {
		binding.rx = C.uintptr_t(rx)
	}

	if cgoLimit != 0 {
		binding.cgoLimiter = make(chan struct{}, cgoLimit)
	}
	if len(u.Path) != 0 && u.Path != "/" {
		err := binding.EnableStorage(u.Path)
		if err != nil {
			return err
		}
	}

	return err2go(C.reindexer_init_system_namespaces(binding.rx))
}

func (binding *Builtin) Clone() bindings.RawBinding {
	return &Builtin{}
}

func (binding *Builtin) Ping() error {
	return err2go(C.reindexer_ping(binding.rx))
}

func (binding *Builtin) ModifyItem(nsHash int, namespace string, format int, data []byte, mode int, precepts []string, stateToken int, txID int) (bindings.RawBuffer, error) {
	if binding.cgoLimiter != nil {
		binding.cgoLimiter <- struct{}{}
		defer func() { <-binding.cgoLimiter }()
	}

	ser1 := cjson.NewPoolSerializer()
	defer ser1.Close()
	ser1.PutVString(namespace)
	ser1.PutVarCUInt(format)
	ser1.PutVarCUInt(mode)
	ser1.PutVarCUInt(stateToken)
	ser1.PutVarCUInt(txID)

	ser1.PutVarCUInt(len(precepts))
	for _, precept := range precepts {
		ser1.PutVString(precept)
	}
	packedArgs := ser1.Bytes()

	return ret2go(C.reindexer_modify_item_packed(binding.rx, buf2c(packedArgs), buf2c(data)))
}

func (binding *Builtin) OpenNamespace(namespace string, enableStorage, dropOnFormatError bool, cacheMode uint8) error {
	var storageOptions bindings.StorageOptions
	storageOptions.Enabled(enableStorage).DropOnFileFormatError(dropOnFormatError)
	opts := C.StorageOpts{
		options: C.uint8_t(storageOptions),
	}
	return err2go(C.reindexer_open_namespace(binding.rx, str2c(namespace), opts, C.uint8_t(cacheMode)))
}
func (binding *Builtin) CloseNamespace(namespace string) error {
	return err2go(C.reindexer_close_namespace(binding.rx, str2c(namespace)))
}

func (binding *Builtin) DropNamespace(namespace string) error {
	return err2go(C.reindexer_drop_namespace(binding.rx, str2c(namespace)))
}

func (binding *Builtin) EnableStorage(path string) error {
	l := len(path)
	if l > 0 && path[l-1] != '/' {
		path += "/"
	}
	return err2go(C.reindexer_enable_storage(binding.rx, str2c(path)))
}

func (binding *Builtin) AddIndex(namespace string, indexDef bindings.IndexDef) error {
	bIndexDef, err := json.Marshal(indexDef)
	if err != nil {
		return err
	}

	sIndexDef := string(bIndexDef)
	err = err2go(C.reindexer_add_index(binding.rx, str2c(namespace), str2c(sIndexDef)))

	return err
}

func (binding *Builtin) UpdateIndex(namespace string, indexDef bindings.IndexDef) error {
	bIndexDef, err := json.Marshal(indexDef)
	if err != nil {
		return err
	}

	sIndexDef := string(bIndexDef)
	err = err2go(C.reindexer_update_index(binding.rx, str2c(namespace), str2c(sIndexDef)))

	return err
}

func (binding *Builtin) DropIndex(namespace, index string) error {
	return err2go(C.reindexer_drop_index(binding.rx, str2c(namespace), str2c(index)))
}

func (binding *Builtin) PutMeta(namespace, key, data string) error {
	return err2go(C.reindexer_put_meta(binding.rx, str2c(namespace), str2c(key), str2c(data)))
}

func (binding *Builtin) GetMeta(namespace, key string) (bindings.RawBuffer, error) {
	return ret2go(C.reindexer_get_meta(binding.rx, str2c(namespace), str2c(key)))
}

func (binding *Builtin) Select(query string, withItems bool, ptVersions []int32, fetchCount int) (bindings.RawBuffer, error) {
	if binding.cgoLimiter != nil {
		binding.cgoLimiter <- struct{}{}
		defer func() { <-binding.cgoLimiter }()
	}
	return ret2go(C.reindexer_select(binding.rx, str2c(query), bool2cint(withItems), (*C.int32_t)(unsafe.Pointer(&ptVersions[0])), C.int(len(ptVersions))))
}

func (binding *Builtin) SelectQuery(data []byte, withItems bool, ptVersions []int32, fetchCount int) (bindings.RawBuffer, error) {
	if binding.cgoLimiter != nil {
		binding.cgoLimiter <- struct{}{}
		defer func() { <-binding.cgoLimiter }()
	}
	return ret2go(C.reindexer_select_query(binding.rx, buf2c(data), bool2cint(withItems), (*C.int32_t)(unsafe.Pointer(&ptVersions[0])), C.int(len(ptVersions))))
}

func (binding *Builtin) DeleteQuery(nsHash int, data []byte) (bindings.RawBuffer, error) {
	if binding.cgoLimiter != nil {
		binding.cgoLimiter <- struct{}{}
		defer func() { <-binding.cgoLimiter }()
	}
	return ret2go(C.reindexer_delete_query(binding.rx, buf2c(data)))
}

func (binding *Builtin) Commit(namespace string) error {
	return err2go(C.reindexer_commit(binding.rx, str2c(namespace)))
}

// CGoLogger logger function for C
//export CGoLogger
func CGoLogger(level int, msg string) {
	logMtx.Lock()
	defer logMtx.Unlock()
	if logger != nil {
		logger.Printf(level, "%s", msg)
	}
}

func (binding *Builtin) EnableLogger(log bindings.Logger) {
	logMtx.Lock()
	defer logMtx.Unlock()
	logger = log
	C.reindexer_enable_go_logger()
}

func (binding *Builtin) DisableLogger() {
	logMtx.Lock()
	defer logMtx.Unlock()
	C.reindexer_disable_go_logger()
	logger = nil
}

func (binding *Builtin) Finalize() error {
	C.destroy_reindexer(binding.rx)
	binding.rx = 0
	return nil
}

func (binding *Builtin) Status() (status bindings.Status) {
	return bindings.Status{
		Builtin: bindings.StatusBuiltin{
			CGOLimit: cap(binding.cgoLimiter),
			CGOUsage: len(binding.cgoLimiter),
		},
	}
}

func newBufFreeBatcher() (bf *bufFreeBatcher) {
	bf = &bufFreeBatcher{
		bufs:   make([]*RawCBuffer, 0, 100),
		bufs2:  make([]*RawCBuffer, 0, 100),
		kickCh: make(chan struct{}, 1),
	}
	go bf.loop()
	return
}

type bufFreeBatcher struct {
	bufs   []*RawCBuffer
	bufs2  []*RawCBuffer
	cbufs  []C.reindexer_resbuffer
	lock   sync.Mutex
	kickCh chan struct{}
}

func (bf *bufFreeBatcher) loop() {
	for {
		<-bf.kickCh

		bf.lock.Lock()
		if len(bf.bufs) == 0 {
			bf.lock.Unlock()
			continue
		}
		bf.bufs, bf.bufs2 = bf.bufs2, bf.bufs
		bf.lock.Unlock()

		for _, buf := range bf.bufs2 {
			bf.cbufs = append(bf.cbufs, buf.cbuf)
		}

		C.reindexer_free_buffers(&bf.cbufs[0], C.int(len(bf.cbufs)))

		for _, buf := range bf.bufs2 {
			buf.cbuf.results_ptr = 0
			bf.toPool(buf)
		}
		bf.cbufs = bf.cbufs[:0]
		bf.bufs2 = bf.bufs2[:0]
	}
}

func (bf *bufFreeBatcher) add(buf *RawCBuffer) {
	if buf.cbuf.results_ptr != 0 {
		bf.toFree(buf)
	} else {
		bf.toPool(buf)
	}
}

func (bf *bufFreeBatcher) toFree(buf *RawCBuffer) {
	bf.lock.Lock()
	bf.bufs = append(bf.bufs, buf)
	bf.lock.Unlock()
	select {
	case bf.kickCh <- struct{}{}:
	default:
	}
}

func (bf *bufFreeBatcher) toPool(buf *RawCBuffer) {
	bufPool.Put(buf)
}
