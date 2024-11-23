package jsonrpc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/pkg/errors"
)

// Params is an ARRAY of json.RawMessages.  This is because *Ethereum* RPCs always use
// arrays is their input parameter; this differs from the official JSONRPC spec, which allows
// parameters of any type.
// But, this assumption makes handling Params in our Ethereum API use-cases *so* much easier.
type Param json.RawMessage
type Params []Param

// MarshalJSON returns m as the JSON encoding of m.
func (m Param) MarshalJSON() ([]byte, error) {
	if m == nil {
		return []byte("null"), nil
	}
	return m, nil
}

// UnmarshalJSON sets *m to a copy of data.
func (m *Param) UnmarshalJSON(data []byte) error {
	if m == nil {
		return errors.New("json.RawMessage: UnmarshalJSON on nil pointer")
	}
	*m = append((*m)[0:0], data...)
	return nil
}

// MustParams can be used to generate JSONRPC Params field from well-known
// data, which should not fail.
//
// Examples:
//
//	request.Params = jsonrpc.MustParams("latest", true)
func MustParams(params ...any) Params {
	out, err := MakeParams(params...)
	if err != nil {
		panic(err)
	}

	return out
}

// MakeParams generates JSONRPC parameters from its inputs, and should be used for
// complex dynamic data which may fail to marshal, in which case the error is propagated
// to the caller.
//
// Examples:
//
//	params, err := jsonrpc.MakeParams(someComplexObject, "string", true)
func MakeParams(params ...any) (Params, error) {
	if len(params) == 0 {
		return nil, nil
	}

	out := make(Params, len(params))
	for i, param := range params {
		b, err := json.Marshal(param)
		if err != nil {
			return nil, err
		}

		out[i] = Param(b)
	}
	return out, nil
}

// UnmarshalInto will decode Params into the passed in values, which
// must be pointer receivers.  The type of the passed in value is used to Unmarshal the data.
// UnmarshalInto will fail if the parameters cannot be converted to the passed-in types.
//
// Example:
//
//	var blockNum string
//	var fullBlock bool
//	err := request.Params.UnmarshalInto(&blockNum, &fullBlock)
//
// IMPORTANT: While Go will compile with non-pointer receivers, the Unmarshal attempt will
// *always* fail with an error.
func (p Params) UnmarshalInto(receivers ...any) error {
	if p == nil {
		return nil
	}

	if len(p) < len(receivers) {
		return errors.New("not enough params to decode")
	}

	for i, r := range receivers {
		err := json.Unmarshal(p[i], r)
		if err != nil {
			return err
		}
	}

	return nil
}

func (p Params) UnmarshalValue(types []reflect.Type) (args []reflect.Value, err error) {
	defer func() {
		if err == nil {
			//Set any missing args to nil. prevent fn call panic
			for i := len(args); i < len(types); i++ {
				if types[i].Kind() != reflect.Ptr {
					args = nil
					err = fmt.Errorf("missing value for required argument %d", i+1)
					return
				}
				args = append(args, reflect.Zero(types[i]))
			}
		}
	}()
	if p == nil {
		return nil, nil
	}
	if len(p) > len(types) {
		return nil, errors.New(fmt.Sprintf("too many arguments, want %d", len(types)))
	}

	args = make([]reflect.Value, 0, len(types))
	for i, _ := range p {
		dec := json.NewDecoder(bytes.NewReader(p[i]))
		argval := reflect.New(types[i])
		if err := dec.Decode(argval.Interface()); err != nil {
			return args, fmt.Errorf("invalid argument %d: %v", i+1, err)
		}
		if argval.IsNil() && types[i].Kind() != reflect.Ptr {
			return args, fmt.Errorf("missing value for required argument %d", i+1)
		}
		args = append(args, argval.Elem())
	}

	return args, nil
}

// UnmarshalSingleParam can be used in the (rare) case where only one of the Request.Params is
// needed.  For example we use this in Smart Routing to extract the blockNum value from RPCs without
// decoding the entire Params array.
//
// Example:
//
//	err := request.Params.UnmarshalSingleParam(pos, &blockNum)
func (p Params) UnmarshalSingleParam(pos int, receiver any) error {
	if pos > (len(p) - 1) {
		return errors.New("not enough parameters to decode position")
	}

	param := p[pos]
	err := json.Unmarshal(param, receiver)
	return err
}
