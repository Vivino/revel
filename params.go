// Copyright (c) 2012-2017 The Revel Framework Authors, All rights reserved.
// Revel Framework source code and usage is governed by a MIT style
// license that can be found in the LICENSE file.

package revel

import (
	"encoding/json"
	"errors"
	"io"
	"io/ioutil"
	"mime/multipart"
	"net/url"
	"os"
	"reflect"

	"github.com/Vivino/go-tools/contx"
)

var _50MB int64 = 50<<20

// Params provides a unified view of the request params.
// Includes:
// - URL query string
// - Form values
// - File uploads
//
// Warning: param maps other than Values may be nil if there were none.
type Params struct {
	url.Values // A unified view of all the individual param maps below.

	// Set by the router
	Fixed url.Values // Fixed parameters from the route, e.g. App.Action("fixed param")
	Route url.Values // Parameters extracted from the route,  e.g. /customers/{id}

	// Set by the ParamsFilter
	Query url.Values // Parameters from the query string, e.g. /index?limit=10
	Form  url.Values // Parameters from the request body.

	Files    map[string][]*multipart.FileHeader // Files uploaded in a multipart form
	tmpFiles []*os.File                         // Temp files used during the request.
	JSON     []byte                             // JSON data from request body
}

var paramsLogger = RevelLog.New("section", "params")

// ParseParams parses the `http.Request` params into `revel.Controller.Params`
func ParseParams(params *Params, req *Request) {
	params.Query = req.GetQuery()

	// Parse the body depending on the content type.
	switch req.ContentType {
	case "application/x-www-form-urlencoded":
		// Typical form.
		var err error
		if params.Form, err = req.GetForm(); err != nil {
			paramsLogger.Warn("ParseParams: Error parsing request body", "error", err)
		}

	case "multipart/form-data":
		// Multipart form.
		if mp, err := req.GetMultipartForm(); err != nil {
			paramsLogger.Warn("ParseParams: parsing request body:", "error", err)
		} else {
			params.Form = mp.GetValues()
			params.Files = mp.GetFiles()
		}
	case "application/json":
		fallthrough
	case "text/json":
		populateParamsJSON(params, req)
	}

	params.Values = params.calcValues()
}

func populateParamsJSON(params *Params, req *Request) {
	body := req.GetBody()
	if body == nil {
		contx.LogFields(req.Context(), "method", req.Method, "url", req.URL).
			Warn("json post received with empty body")
		return
	}
	content, err := ioutil.ReadAll(LimitReader(body, _50MB))
	if err != nil {
		if !errors.Is(err, io.EOF) {
			contx.LogCause(req.Context(), err, "method", req.Method, "url", req.URL).
				Error("failed to read JSON body")
			params.JSON = nil
		}
	}
	params.JSON = content
}

type limitReader struct {
	R io.Reader
	N int64
}

// Read is an implementation of io.Reader::Read for limitReader, reading the next chunk
// of bytes using the inner reader 'R'. If the specified limit is reached, Read returns
// an error reflecting this.
func (l *limitReader) Read(p []byte) (int, error) {
	if int64(len(p)) > l.N {
		p = p[0:l.N]
	}
	n, err := l.R.Read(p)
	if l.N <= 0 {
		if err != io.EOF {
			return n, errors.New("content larger than maximum limit")
		}
	}

	l.N -= int64(n)
	return n, err
}

var _ io.Reader = &limitReader{}

// LimitReader is a custom implementation of io.LimitReader, which will return and error when the byte
// stream is larger than the given limit, rather than returning EOF.
// The limit provided, is an inclusive limit, meaning that should you set the limit to 1MB, the reader
// will read any file with a size up to, and including 1MB.
func LimitReader(reader io.Reader, limit int64) io.Reader {
	return &limitReader{reader, limit + 1}
}

// Bind looks for the named parameter, converts it to the requested type, and
// writes it into "dest", which must be settable.  If the value can not be
// parsed, "dest" is set to the zero value.
func (p *Params) Bind(dest interface{}, name string) {
	value := reflect.ValueOf(dest)
	if value.Kind() != reflect.Ptr {
		paramsLogger.Panic("Bind: revel/params: non-pointer passed to Bind: " + name)
	}
	value = value.Elem()
	if !value.CanSet() {
		paramsLogger.Panic("Bind: revel/params: non-settable variable passed to Bind: " + name)
	}

	// Remove the json from the Params, this will stop the binder from attempting
	// to use the json data to populate the destination interface. We do not want
	// to do this on a named bind directly against the param, it is ok to happen when
	// the action is invoked.
	jsonData := p.JSON
	p.JSON = nil
	value.Set(Bind(p, name, value.Type()))
	p.JSON = jsonData
}

// Bind binds the JSON data to the dest.
func (p *Params) BindJSON(dest interface{}) error {
	value := reflect.ValueOf(dest)
	if value.Kind() != reflect.Ptr {
		paramsLogger.Warn("BindJSON: Not a pointer")
		return errors.New("BindJSON not a pointer")
	}
	if err := json.Unmarshal(p.JSON, dest); err != nil {
		paramsLogger.Warn("BindJSON: Unable to unmarshal request:", "error", err)
		return err
	}
	return nil
}

// calcValues returns a unified view of the component param maps.
func (p *Params) calcValues() url.Values {
	numParams := len(p.Query) + len(p.Fixed) + len(p.Route) + len(p.Form)

	// If there were no params, return an empty map.
	if numParams == 0 {
		return make(url.Values, 0)
	}

	// If only one of the param sources has anything, return that directly.
	switch numParams {
	case len(p.Query):
		return p.Query
	case len(p.Route):
		return p.Route
	case len(p.Fixed):
		return p.Fixed
	case len(p.Form):
		return p.Form
	}

	// Copy everything into a param map,
	// order of priority is least to most trusted
	values := make(url.Values, numParams)

	// ?query string parameters are first
	for k, v := range p.Query {
		values[k] = append(values[k], v...)
	}

	// form parameters append
	for k, v := range p.Form {
		values[k] = append(values[k], v...)
	}

	// :/path parameters overwrite
	for k, v := range p.Route {
		values[k] = v
	}

	// fixed route parameters overwrite
	for k, v := range p.Fixed {
		values[k] = v
	}

	return values
}

func ParamsFilter(c *Controller, fc []Filter) {
	ParseParams(c.Params, c.Request)

	// Clean up from the request.
	defer func() {
		for _, tmpFile := range c.Params.tmpFiles {
			err := os.Remove(tmpFile.Name())
			if err != nil {
				paramsLogger.Warn("ParamsFilter: Could not remove upload temp file:", err)
			}
		}
	}()

	fc[0](c, fc[1:])
}
