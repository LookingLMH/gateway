package proxy

import (
	"github.com/buger/jsonparser"
	"github.com/fagongzi/log"
	"github.com/valyala/fasthttp"
)

var (
	emptyString = []byte{'"', '"'}
	emptyObject = []byte("{}")
	emptyArray  = []byte("[]")
)

type render struct {
	multi        bool
	multiContext []byte
	api          *apiRuntime
	nodes        []*dispathNode
	doRender     func(*fasthttp.RequestCtx)
	allocBytes   [][]byte
}

func (rd *render) init(api *apiRuntime, nodes []*dispathNode) {
	rd.nodes = nodes
	rd.api = api
	rd.doRender = rd.renderSingle

	if len(nodes) > 1 {
		rd.doRender = rd.renderMulti
	} else if len(nodes) == 0 {
		rd.doRender = rd.renderDefault
	}
}

func (rd *render) reset() {
	for _, buf := range rd.allocBytes {
		bytesPool.Free(buf)
	}
	*rd = emptyRender
}

func (rd *render) render(ctx *fasthttp.RequestCtx, multiCtx *multiContext) {
	ctx.Response.Header.SetContentType(MultiResultsContentType)
	ctx.SetStatusCode(fasthttp.StatusOK)
	if multiCtx != nil {
		rd.multiContext = multiCtx.data
	}
	rd.doRender(ctx)
}

func (rd *render) renderSingle(ctx *fasthttp.RequestCtx) {
	dn := rd.nodes[0]

	if dn.res != nil {
		ctx.SetStatusCode(dn.res.StatusCode())
	}

	if dn.err != nil ||
		dn.code >= fasthttp.StatusBadRequest {
		log.Errorf("render: render failed, code=<%d>, errors:\n%+v",
			dn.code,
			dn.err)

		if rd.api.hasDefaultValue() {
			rd.renderDefault(ctx)
			dn.release()
			return
		}

		ctx.SetStatusCode(dn.code)
		dn.release()
		return
	}

	if !rd.api.hasRenderTemplate() {
		rd.renderRaw(ctx, dn)
		return
	}

	src := dn.getResponseBody()
	dn.release()

	rd.renderTemplate(ctx, src)
}

func (rd *render) renderMulti(ctx *fasthttp.RequestCtx) {
	var err error
	var hasError bool
	code := fasthttp.StatusInternalServerError
	hasTemplate := rd.api.hasRenderTemplate()

	for _, dn := range rd.nodes {
		if hasError {
			dn.release()
			continue
		}

		if dn.hasError() &&
			!dn.hasDefaultValue() {
			hasError = true
			code = dn.code
			err = dn.err
			dn.release()
			continue
		}

		dn.copyHeaderTo(ctx)
		dn.release()
	}

	if hasError {
		log.Errorf("render: render failed, code=<%d>, errors:\n%+v",
			code,
			err)

		if rd.api.hasDefaultValue() {
			rd.renderDefault(ctx)
			return
		}

		ctx.SetStatusCode(code)
		return
	}

	if !hasTemplate {
		ctx.Write(rd.multiContext)
		return
	}

	rd.renderTemplate(ctx, rd.multiContext)
}

func (rd *render) renderRaw(ctx *fasthttp.RequestCtx, dn *dispathNode) {
	ctx.Response.Header.SetContentTypeBytes(dn.getResponseContentType())
	ctx.Write(dn.getResponseBody())
	dn.release()
}

func (rd *render) renderDefault(ctx *fasthttp.RequestCtx) {
	if !rd.api.hasDefaultValue() {
		return
	}

	header := &ctx.Response.Header

	for _, h := range rd.api.meta.DefaultValue.Headers {
		if h.Name == "Content-Type" {
			header.SetContentType(h.Value)
		} else {
			header.Add(h.Name, h.Value)
		}
	}

	for _, ck := range rd.api.defaultCookies {
		header.SetCookie(ck)
	}

	ctx.SetStatusCode(fasthttp.StatusOK)
	ctx.Write(rd.api.meta.DefaultValue.Body)
}

func (rd *render) renderTemplate(ctx *fasthttp.RequestCtx, context []byte) {
	data, err := rd.extract(context)
	if err != nil {
		log.Errorf("render: render failed, errors:\n%+v",
			err)
		ctx.SetStatusCode(fasthttp.StatusInternalServerError)
		return
	}

	ctx.Write(data)
}

func (rd *render) extract(src []byte) ([]byte, error) {
	var err error
	data := emptyObject
	for _, obj := range rd.api.parsedRenderObjects {
		isFlat := obj.meta.FlatAttrs
		tmp := emptyObject

		for _, attr := range obj.attrs {
			value, err := rd.extractValue(attr, src)
			if err != nil {
				return nil, err
			}

			// if is flat attr, add to data
			// otherwise, add to tmp object, and add tmp obj to data
			if isFlat {
				if len(value) > 0 && attr.meta.Name != "" {
					data, err = jsonparser.Set(data, value, attr.meta.Name)
					if err != nil {
						return nil, err
					}
				}

				continue
			}

			if len(value) > 0 && attr.meta.Name != "" {
				tmp, err = jsonparser.Set(tmp, value, attr.meta.Name)
				if err != nil {
					return nil, err
				}
			}
		}

		if !isFlat && len(tmp) > 0 && obj.meta.Name != "" {
			data, err = jsonparser.Set(data, tmp, obj.meta.Name)
			if err != nil {
				return nil, err
			}
		}
	}

	return data, nil
}

func (rd *render) extractValue(attr *renderAttr, src []byte) ([]byte, error) {
	if len(attr.extracts) == 1 {
		return rd.extractAttrValue(src, attr.extracts[0]...)
	}

	obj := emptyObject
	for _, exp := range attr.extracts {
		data, err := rd.extractAttrValue(src, exp...)
		if err != nil {
			return nil, err
		}

		if len(data) > 0 && len(exp) > 0 {
			obj, err = jsonparser.Set(obj, data, exp[len(exp)-1])
			if err != nil {
				return nil, err
			}
		}
	}
	return obj, nil
}

func (rd *render) extractAttrValue(src []byte, paths ...string) ([]byte, error) {
	value, vt, _, err := jsonparser.Get(src, paths...)
	if err != nil {
		return nil, err
	}

	size := len(value)
	if vt == jsonparser.String && size > 0 {
		stringValue := bytesPool.Alloc(size + 2)
		rd.allocBytes = append(rd.allocBytes, stringValue)
		stringValue[0] = '"'
		copy(stringValue[1:], value)
		stringValue[size+1] = '"'
		return stringValue, nil
	} else if vt == jsonparser.String && size == 0 {
		return emptyString, nil
	} else if vt == jsonparser.Array && size == 0 {
		return emptyArray, nil
	} else if vt == jsonparser.Unknown {
		return emptyString, nil
	} else if vt == jsonparser.NotExist {
		return emptyString, nil
	} else if vt == jsonparser.Null {
		return emptyString, nil
	}

	return value, nil
}
