// Package plgo provides a Perl runtime to Go
package plgo

//go:generate ./gen.pl $GOFILE

/*
#cgo CFLAGS: -Wall -D_REENTRANT -D_GNU_SOURCE -DDEBIAN -fstack-protector -fno-strict-aliasing -pipe -I/usr/local/include -D_LARGEFILE_SOURCE -D_FILE_OFFSET_BITS=64  -I/usr/lib/perl/5.18/CORE
#cgo LDFLAGS: -Wl,-E  -fstack-protector -L/usr/local/lib  -L/usr/lib/perl/5.18/CORE -lperl -ldl -lm -lpthread -lc -lcrypt
#include "glue.h"
*/
import "C"
import (
	"fmt"
	"reflect"
	"runtime"
	"sync"
	"unsafe"
)

// PL holds a Perl runtime
type PL struct {
	thx        C.tTHX
	mx         sync.Mutex
	newSVcmplx func(float64, float64) *sV
	valSVcmplx func(*sV) (float64, float64)
}

var activePL *PL

type sV struct {
	pl  *PL
	sv  *C.SV
	own bool
}

// We can not reliably hold pointers to Go objects in C
// https://github.com/golang/go/issues/12416 documents the rules.
// runtime.GC() can move objects in memory so we have to create an
// indirection layer.  The liveVals map will serve this purpose.
var (
	liveValsSeq = 0
	liveVals    = map[int]*reflect.Value{}
)

func plFini(pl *PL) {
	pl.mx.Lock()
	C.glue_fini(pl.thx)
	pl.mx.Unlock()
}

// New initializes a Perl runtime
func New() *PL {
	pl := new(PL)
	pl.thx = C.glue_init()
	runtime.SetFinalizer(pl, plFini)
	return pl
}

// Eval will execute a string of Perl code.  If ptrs are provided,
// the list of results from Perl will be stored in the list of ptrs.
// Not all types are supported, but many basic types are, including
// functions.
func (pl *PL) Eval(text string, ptrs ...interface{}) error {
	var err *C.SV
	prevPL := activePL
	activePL = pl
	code := C.CString("[ do { \n#line 1 \"plgo.Eval()\"\n" + text + "\n } ]")
	pl.mx.Lock()
	av := C.glue_eval(pl.thx, code, &err)
	pl.mx.Unlock()
	activePL = prevPL

	if err != nil {
		e := pl.sV(err, true)
		pl.mx.Lock()
		C.glue_dec(pl.thx, av)
		C.glue_dec(pl.thx, err)
		pl.mx.Unlock()
		return e
	}
	if len(ptrs) > 0 {
		i := 0
		cb := func(sv *C.SV) {
			if i >= len(ptrs) {
				return
			}
			pl.mx.Unlock()
			val := reflect.ValueOf(ptrs[i]).Elem()
			pl.valSV(&val, sv)
			pl.mx.Lock()
			i++
		}
		pl.mx.Lock()
		C.glue_walkAV(pl.thx, av, C.IV(uintptr(unsafe.Pointer(&cb))))
		pl.mx.Unlock()
	}
	pl.mx.Lock()
	C.glue_dec(pl.thx, av)
	pl.mx.Unlock()
	return nil
}

// Live counts the number of live variables in the Perl instance.
// This function is used for leak detection in the test code.
// runtime.GC() must be called to get accurate live value counts.
func (pl *PL) Live() int {
	pl.mx.Lock()
	defer pl.mx.Unlock()
	return int(C.glue_count_live(pl.thx))
}

func (pl *PL) newSVval(src reflect.Value) *C.SV {
	t := src.Type()
	switch src.Kind() {
	case reflect.Bool:
		pl.mx.Lock()
		defer pl.mx.Unlock()
		return C.glue_newBool(pl.thx, C.bool(src.Bool()))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		pl.mx.Lock()
		defer pl.mx.Unlock()
		return C.glue_newIV(pl.thx, C.IV(src.Int()))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		pl.mx.Lock()
		defer pl.mx.Unlock()
		return C.glue_newUV(pl.thx, C.UV(src.Uint()))
	case reflect.Uintptr:
		pl.mx.Lock()
		defer pl.mx.Unlock()
		return C.glue_newUV(pl.thx, C.UV(src.Uint()))
	case reflect.Float32, reflect.Float64:
		pl.mx.Lock()
		defer pl.mx.Unlock()
		return C.glue_newNV(pl.thx, C.NV(src.Float()))
	case reflect.Complex64, reflect.Complex128:
		if pl.newSVcmplx == nil {
			pl.Eval(`
				require Math::Complex;
				sub { my $rv = Math::Complex->new(0, 0); $rv->_set_cartesian([ @_ ]); return $rv; }
			`, &pl.newSVcmplx)
		}
		v := src.Complex()
		return pl.newSVcmplx(real(v), imag(v)).sv
	case reflect.Array,
		reflect.Slice:
		dst := make([]*C.SV, 1+src.Len())
		for i := 0; i < src.Len(); i++ {
			dst[i] = pl.newSVval(src.Index(i))
		}
		pl.mx.Lock()
		defer pl.mx.Unlock()
		return C.glue_newAV(pl.thx, &dst[0])
	case reflect.Chan:
	case reflect.Func:
		liveValsSeq++
		id := liveValsSeq
		liveVals[id] = &src
		pl.mx.Lock()
		defer pl.mx.Unlock()
		return C.glue_newCV(pl.thx, C.IV(id), C.IV(t.NumIn()), C.IV(t.NumOut()))
	case reflect.Interface:
	case reflect.Map:
		keys := src.MapKeys()
		dst := make([]*C.SV, 1+2*len(keys))
		for i, key := range keys {
			dst[i<<1] = pl.newSVval(key)
			dst[i<<1+1] = pl.newSVval(src.MapIndex(key))
		}
		pl.mx.Lock()
		defer pl.mx.Unlock()
		return C.glue_newHV(pl.thx, &dst[0])
	case reflect.Ptr:
		// TODO: *sV handling is a special case, but generic Ptr support
		// could be implemented
		if t == reflect.TypeOf((*sV)(nil)) {
			return src.Interface().(*sV).sv
		}
	case reflect.String:
		str := src.String()
		pl.mx.Lock()
		defer pl.mx.Unlock()
		return C.glue_newPV(pl.thx, C.CString(str), C.STRLEN(len(str)))
	case reflect.Struct:
	case reflect.UnsafePointer:
	}
	panic("unhandled type \"" + src.Kind().String() + "\"")
	return nil
}

//export goStepHV
func goStepHV(cb uintptr, key *C.SV, val *C.SV) {
	//the callee is responsible for unlocking the pl.mx
	(*(*func(*C.SV, *C.SV))(unsafe.Pointer(cb)))(key, val)
}

//export goStepAV
func goStepAV(cb uintptr, sv *C.SV) {
	//the callee is responsible for unlocking the pl.mx
	(*(*func(*C.SV))(unsafe.Pointer(cb)))(sv)
}

func (pl *PL) valSV(dst *reflect.Value, src *C.SV) {
	t := dst.Type()
	switch t.Kind() {
	case reflect.Bool:
		pl.mx.Lock()
		defer pl.mx.Unlock()
		dst.SetBool(bool(C.glue_getBool(pl.thx, src)))
		return
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		pl.mx.Lock()
		defer pl.mx.Unlock()
		dst.SetInt(int64(C.glue_getIV(pl.thx, src)))
		return
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		pl.mx.Lock()
		defer pl.mx.Unlock()
		dst.SetUint(uint64(C.glue_getUV(pl.thx, src)))
		return
	case reflect.Uintptr:
		pl.mx.Lock()
		defer pl.mx.Unlock()
		dst.SetUint(uint64(C.glue_getUV(pl.thx, src)))
		return
	case reflect.Float32, reflect.Float64:
		pl.mx.Lock()
		defer pl.mx.Unlock()
		dst.SetFloat(float64(C.glue_getNV(pl.thx, src)))
		return
	case reflect.Complex64, reflect.Complex128:
		if pl.valSVcmplx == nil {
			pl.Eval(`
				require Math::Complex;
				sub { return Math::Complex::Re($_[0]), Math::Complex::Im($_[0]); }
			`, &pl.valSVcmplx)
		}
		re, im := pl.valSVcmplx(pl.sV(src, false))
		dst.SetComplex(complex128(complex(re, im)))
		return
	case reflect.Array:
	case reflect.Chan:
	case reflect.Func:
		var cv *sV
		dst.Set(reflect.MakeFunc(t, func(arg []reflect.Value) (ret []reflect.Value) {
			args := make([]*C.SV, 1+t.NumIn())
			for i, val := range arg {
				args[i] = pl.newSVval(val)
			}
			rets := make([]*C.SV, 1+t.NumOut())

			prevPL := activePL
			activePL = pl
			pl.mx.Lock()
			err := C.glue_call_sv(pl.thx, cv.sv,
				&args[0], &rets[0], C.int(t.NumOut()))
			pl.mx.Unlock()
			activePL = prevPL

			ret = make([]reflect.Value, t.NumOut())

			if err == nil {
				// copy Perl rets to Go, zero out error rvs
				j := 0
				for i, _ := range ret {
					if t.Out(i) == reflect.TypeOf((*error)(nil)).Elem() {
						ret[i] = reflect.Zero(t.Out(i))
					} else {
						ret[i] = reflect.New(t.Out(i)).Elem()
						pl.valSV(&ret[i], rets[j])
						j++
					}
				}
			} else {
				// copy Perl error to Go, zero out data rvs
				err_found := false
				for i, _ := range ret {
					if t.Out(i) == reflect.TypeOf((*error)(nil)).Elem() {
						ret[i] = reflect.New(t.Out(i)).Elem()
						pl.valSV(&ret[i], err)
						err_found = true
					} else {
						ret[i] = reflect.Zero(t.Out(i))
					}
				}
				if !err_found {
					panic(pl.sV(err, true))
				}
			}
			pl.mx.Lock()
			for _, sv := range rets[0 : len(rets)-1] {
				C.glue_dec(pl.thx, sv)
			}
			if err != nil {
				C.glue_dec(pl.thx, err)
			}
			pl.mx.Unlock()
			return
		}))
		cv = pl.sV(src, true)
		return
	case reflect.Interface:
		if dst.Type() == reflect.TypeOf((*error)(nil)).Elem() {
			dst.Set(reflect.ValueOf(pl.sV(src, true)))
			return
		}
	case reflect.Map:
		dst.Set(reflect.MakeMap(t))
		cb := func(key *C.SV, val *C.SV) {
			pl.mx.Unlock()
			k := reflect.New(t.Key()).Elem()
			pl.valSV(&k, key)
			v := reflect.New(t.Elem()).Elem()
			pl.valSV(&v, val)
			dst.SetMapIndex(k, v)
			pl.mx.Lock()
		}
		pl.mx.Lock()
		C.glue_walkHV(pl.thx, src, C.IV(uintptr(unsafe.Pointer(&cb))))
		pl.mx.Unlock()
		return
	case reflect.Ptr:
		// TODO: for now we're only handling *plgo.sV wrapping
		if dst.Type() == reflect.TypeOf((*sV)(nil)) {
			dst.Set(reflect.ValueOf(pl.sV(src, false)))
			return
		}
	case reflect.Slice:
		// TODO: this is sketchy, refactor
		tmp := reflect.MakeSlice(t, 0, 0)
		cb := func(sv *C.SV) {
			pl.mx.Unlock()
			val := reflect.New(t.Elem()).Elem()
			pl.valSV(&val, sv)
			tmp = reflect.Append(tmp, val)
			pl.mx.Lock()
		}
		pl.mx.Lock()
		C.glue_walkAV(pl.thx, src, C.IV(uintptr(unsafe.Pointer(&cb))))
		pl.mx.Unlock()
		dst.Set(tmp)
		return
	case reflect.String:
		var len C.STRLEN
		pl.mx.Lock()
		cs := C.glue_getPV(pl.thx, src, &len)
		pl.mx.Unlock()
		dst.SetString(C.GoStringN(cs, C.int(len)))
		return
	case reflect.Struct:
	case reflect.UnsafePointer:
	}
	panic("unhandled type \"" + t.Kind().String() + "\"")
}

func svFini(sv *sV) {
	if sv.own {
		if false {
			fmt.Printf("RELEASE %p\n", sv.sv)
		}
		sv.pl.mx.Lock()
		C.glue_dec(sv.pl.thx, sv.sv)
		sv.pl.mx.Unlock()
	}
}

func (pl *PL) sV(sv *C.SV, own bool) *sV {
	var self sV
	self.pl = pl
	self.sv = sv
	self.own = own
	pl.mx.Lock()
	C.glue_inc(pl.thx, sv)
	pl.mx.Unlock()
	runtime.SetFinalizer(&self, svFini)
	if self.own && false {
		pl.mx.Lock()
		C.glue_track(pl.thx, sv)
		pl.mx.Unlock()
		fmt.Printf("ACQUIRE %p\n", self.sv)
	}
	return &self
}

func (sv *sV) Error() string {
	v := reflect.New(reflect.TypeOf((*string)(nil)).Elem()).Elem()
	sv.pl.valSV(&v, sv.sv)
	return v.String()
}

//export goInvoke
func goInvoke(data int, arg unsafe.Pointer, ret unsafe.Pointer) {
	if activePL == nil {
		panic("activePL not set")
	}
	activePL.mx.Unlock()
	cnv := func(raw unsafe.Pointer, n int) []*C.SV {
		return *(*[]*C.SV)(unsafe.Pointer(&reflect.SliceHeader{
			Data: uintptr(raw),
			Len:  n,
			Cap:  n,
		}))
	}
	// recover info
	val := liveVals[data]
	t := val.Type()
	// xlate args - they are already mortal, don't take ownership unless
	// they need to survive beyond the function call
	args := make([]reflect.Value, t.NumIn())
	for i, sv := range cnv(arg, len(args)) {
		args[i] = reflect.New(t.In(i)).Elem()
		activePL.valSV(&args[i], sv)
	}
	// xlate rets - return as owning references and glue_invoke() will
	// mortalize them for us
	rets := cnv(ret, t.NumOut())
	for i, val := range val.Call(args) {
		rets[i] = activePL.newSVval(val)
	}
	activePL.mx.Lock()
}

//export goRelease
func goRelease(data int) {
	// if this gets complicated, remember to unlock/lock pl.mx
	delete(liveVals, data)
}
