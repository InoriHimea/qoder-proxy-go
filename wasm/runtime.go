package wasm

import (
	"context"
	cryptorand "crypto/rand"
	"fmt"
	"os"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// ObjKind identifies the type of a host-side simulated JS object.
type ObjKind int

const (
	ObjNull ObjKind = iota
	ObjUndefined
	ObjTrue
	ObjFalse
	ObjString
	ObjNumber
	ObjBoolean
	ObjMap
	ObjUint8Array
	ObjFunction
	ObjCrypto
	ObjWindow
	ObjGlobalThis
	ObjError
	ObjProcess
)

// Obj represents a simulated JS value stored in the reference table.
type Obj struct {
	kind    ObjKind
	str     string
	num     float64
	boo     bool
	members map[string]*Obj
	data    []byte
}

func (o *Obj) Clone() *Obj {
	if o == nil {
		return nil
	}
	cp := *o
	if o.members != nil {
		cp.members = make(map[string]*Obj, len(o.members))
		for k, v := range o.members {
			cp.members[k] = v
		}
	}
	if o.data != nil {
		cp.data = append([]byte(nil), o.data...)
	}
	return &cp
}

// refTable manages the simulated JS object reference table used by
// wasm-bindgen glue.  Refs are u32 indices into the table; the first
// four are always null, undefined, true, false per wasm-bindgen spec.
type refTable struct {
	mu    sync.Mutex
	table []*Obj
}

func newRefTable() *refTable {
	return &refTable{
		// Indices 0-3: null, undefined, true, false (wasm-bindgen invariant)
		table: []*Obj{nil, &Obj{kind: ObjUndefined}, &Obj{kind: ObjTrue}, &Obj{kind: ObjFalse}},
	}
}

func (r *refTable) resolve(ref uint32) *Obj {
	if ref == 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if int(ref) >= len(r.table) {
		return nil
	}
	return r.table[ref]
}

func (r *refTable) intern(o *Obj) uint32 {
	r.mu.Lock()
	defer r.mu.Unlock()
	idx := len(r.table)
	r.table = append(r.table, o)
	return uint32(idx)
}

func (r *refTable) cloneRef(ref uint32) uint32 {
	o := r.resolve(ref)
	if o == nil {
		return 0
	}
	return r.intern(o.Clone())
}

func (r *refTable) dropRef(uint32) {
	// no-op; Go GC handles it
}

// Runtime holds the compiled WASM module and all host state.
type Runtime struct {
	ctx     context.Context
	runtime wazero.Runtime
	mod     api.Module
	refTab  *refTable
	mu      sync.Mutex
}

// New creates a Runtime by loading WASM from a file.
func New(ctx context.Context, wasmPath string) (*Runtime, error) {
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		return nil, err
	}
	return newFromBytes(ctx, wasmBytes)
}

// NewFromBytes creates a Runtime from an in-memory WASM module, e.g. one
// embedded via go:embed.
func NewFromBytes(ctx context.Context, wasmBytes []byte) (*Runtime, error) {
	return newFromBytes(ctx, wasmBytes)
}

func newFromBytes(ctx context.Context, wasmBytes []byte) (*Runtime, error) {
	r := &Runtime{
		ctx:     ctx,
		runtime: wazero.NewRuntime(ctx),
		refTab:  newRefTable(),
	}
	if err := r.registerImports(); err != nil {
		r.runtime.Close(ctx)
		return nil, err
	}
	compiled, err := r.runtime.CompileModule(ctx, wasmBytes)
	if err != nil {
		r.runtime.Close(ctx)
		return nil, err
	}
	mod, err := r.runtime.InstantiateModule(ctx, compiled, wazero.NewModuleConfig())
	if err != nil {
		r.runtime.Close(ctx)
		return nil, err
	}
	r.mod = mod
	return r, nil
}

// Close releases the wazero runtime.
func (r *Runtime) Close() {
	r.runtime.Close(r.ctx)
}

// modRef returns the instantiated module.
func (r *Runtime) modRef() api.Module { return r.mod }

// malloc allocates `size` bytes in WASM linear memory via wasm-bindgen's
// __wbindgen_export2 (malloc) and returns the pointer offset.
func (r *Runtime) malloc(size uint32) (uint32, error) {
	fn := r.mod.ExportedFunction("__wbindgen_export2")
	if fn == nil {
		return 0, fmt.Errorf("malloc: __wbindgen_export2 not found")
	}
	results, err := fn.Call(r.ctx, uint64(size), uint64(1)) // align=1 for byte slices
	if err != nil {
		return 0, fmt.Errorf("malloc: %w", err)
	}
	return uint32(results[0]), nil
}

// free deallocates memory previously allocated via malloc.
func (r *Runtime) free(ptr, size uint32) {
	fn := r.mod.ExportedFunction("__wbindgen_export4")
	if fn == nil {
		return
	}
	_, _ = fn.Call(r.ctx, uint64(ptr), uint64(size), 1)
}

// writeString writes a Go string into WASM linear memory by first allocating
// via wasm-bindgen's malloc, then writing the bytes. Returns the pointer offset.
func (r *Runtime) writeString(s string) (uint32, error) {
	ptr, err := r.malloc(uint32(len(s)))
	if err != nil {
		return 0, err
	}
	mem := r.mod.Memory()
	if !mem.Write(ptr, []byte(s)) {
		r.free(ptr, uint32(len(s)))
		return 0, fmt.Errorf("writeString: Write failed")
	}
	return ptr, nil
}

// readString reads a string from WASM linear memory at the given
// offset and length.
func (r *Runtime) readString(idx, length uint32) string {
	mem := r.mod.Memory()
	buf, ok := mem.Read(idx, length)
	if !ok {
		return ""
	}
	return string(buf)
}

// callWasmFunc invokes an exported WASM function.
func (r *Runtime) callWasmFunc(name string, args ...uint64) ([]uint64, error) {
	fn := r.mod.ExportedFunction(name)
	if fn == nil {
		return []uint64{}, fmt.Errorf("no exported function: %s", name)
	}
	return fn.Call(r.ctx, args...)
}

// refTablePtr returns the reference table pointer.
func (r *Runtime) refTablePtr() *refTable { return r.refTab }

func boolToU64(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}

func (r *Runtime) fillRandom(b []byte) {
	if _, err := cryptorand.Read(b); err != nil {
		var counter uint32
		for i := range b {
			counter++
			b[i] = byte(counter ^ (counter >> 7))
		}
	}
}

func objToKey(o *Obj) string {
	if o == nil {
		return "null"
	}
	switch o.kind {
	case ObjUndefined:
		return "undefined"
	case ObjNull:
		return "null"
	case ObjString:
		return o.str
	default:
		return string(rune(128 + o.kind))
	}
}
