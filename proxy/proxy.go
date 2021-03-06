package proxy

import (
	"container/list"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/CodisLabs/codis/pkg/utils/log"
	"github.com/fagongzi/gateway/conf"
	"github.com/fagongzi/gateway/pkg/model"
	"github.com/valyala/fasthttp"
)

const (
	// ErrPrefixRequestCancel user cancel request error
	ErrPrefixRequestCancel = "request canceled"
)

var (
	// ErrNoServer no server
	ErrNoServer = errors.New("has no server")
)

var (
	// HeaderContentType content-type header
	HeaderContentType = "Content-Type"
	// MergeContentType merge operation using content-type
	MergeContentType = "application/json; charset=utf-8"
	// MergeRemoveHeaders merge operation need to remove headers
	MergeRemoveHeaders = []string{
		"Content-Length",
		"Content-Type",
		"Date",
	}
)

// Proxy Proxy
type Proxy struct {
	fastHTTPClient *FastHTTPClient
	config         *conf.Conf
	routeTable     *model.RouteTable
	flushInterval  time.Duration
	filters        *list.List
}

// NewProxy create a new proxy
func NewProxy(config *conf.Conf, routeTable *model.RouteTable) *Proxy {
	p := &Proxy{
		fastHTTPClient: NewFastHTTPClient(config),
		config:         config,
		routeTable:     routeTable,
		filters:        list.New(),
	}

	return p
}

// RegistryFilter registry a filter
func (p *Proxy) RegistryFilter(name string) {
	f, err := newFilter(name, p.config, p)
	if nil != err {
		log.Panicf("Proxy unknow filter <%s>.", name)
	}

	p.filters.PushBack(f)
}

// Start start proxy
func (p *Proxy) Start() {
	err := p.startRPCServer()

	if nil != err {
		log.PanicErrorf(err, "Proxy start rpc at <%s> fail.", p.config.MgrAddr)
	}

	log.ErrorErrorf(fasthttp.ListenAndServe(p.config.Addr, p.ReverseProxyHandler), "Proxy exit at %s", p.config.Addr)
}

// ReverseProxyHandler http reverse handler
func (p *Proxy) ReverseProxyHandler(ctx *fasthttp.RequestCtx) {
	results := p.routeTable.Select(&ctx.Request)

	if nil == results || len(results) == 0 {
		ctx.SetStatusCode(fasthttp.StatusServiceUnavailable)
		return
	}

	count := len(results)
	merge := count > 1

	if merge {
		wg := &sync.WaitGroup{}
		wg.Add(count)

		for _, result := range results {
			result.Merge = merge

			go func(result *model.RouteResult) {
				p.doProxy(ctx, wg, result)
			}(result)
		}

		wg.Wait()
	} else {
		p.doProxy(ctx, nil, results[0])
	}

	for _, result := range results {
		if result.Err != nil {
			ctx.SetStatusCode(result.Code)
			result.Release()
			return
		}

		if !merge {
			p.writeResult(ctx, result.Res)
			result.Release()
			return
		}
	}

	for _, result := range results {
		for _, h := range MergeRemoveHeaders {
			result.Res.Header.Del(h)
		}
		result.Res.Header.CopyTo(&ctx.Response.Header)
	}

	ctx.Response.Header.Add(HeaderContentType, MergeContentType)
	ctx.SetStatusCode(fasthttp.StatusOK)

	ctx.WriteString("{")

	for index, result := range results {
		ctx.WriteString("\"")
		ctx.WriteString(result.Node.AttrName)
		ctx.WriteString("\":")
		ctx.Write(result.Res.Body())
		if index < count-1 {
			ctx.WriteString(",")
		}

		result.Release()
	}

	ctx.WriteString("}")
}

func (p *Proxy) doProxy(ctx *fasthttp.RequestCtx, wg *sync.WaitGroup, result *model.RouteResult) {
	if nil != wg {
		defer wg.Done()
	}

	svr := result.Svr

	if nil == svr {
		result.Err = ErrNoServer
		result.Code = http.StatusServiceUnavailable
		return
	}

	outreq := copyRequest(&ctx.Request)

	// change url
	if result.NeedRewrite() {
		// if not use rewrite, it only change uri path and query string
		realPath := result.GetRealPath(&ctx.Request)
		if "" != realPath {
			log.Infof("URL Rewrite from <%s> to <%s>", string(ctx.URI().FullURI()), realPath)
			outreq.SetRequestURI(realPath)
			outreq.SetHost(svr.Addr)
		}
	} else {
		// if not use rewrite, it only change uri path, the query string will use origin.
		if result.Node != nil {
			outreq.URI().SetPath(result.Node.URL)
		}
	}

	c := &filterContext{
		ctx:        ctx,
		outreq:     outreq,
		result:     result,
		rb:         p.routeTable,
		runtimeVar: make(map[string]string),
	}

	// pre filters
	filterName, code, err := p.doPreFilters(c)
	if nil != err {
		log.WarnErrorf(err, "Proxy Filter-Pre<%s> fail", filterName)
		result.Err = err
		result.Code = code
		return
	}

	c.startAt = time.Now().UnixNano()
	res, err := p.fastHTTPClient.Do(outreq, svr.Addr)
	c.endAt = time.Now().UnixNano()

	result.Res = res

	if err != nil || res.StatusCode() >= fasthttp.StatusInternalServerError {
		resCode := http.StatusServiceUnavailable

		if nil != err {
			log.InfoErrorf(err, "Proxy Fail <%s>", svr.Addr)
		} else {
			resCode = res.StatusCode()
			log.InfoErrorf(err, "Proxy Fail <%s>, Code <%d>", svr.Addr, res.StatusCode())
		}

		// 用户取消，不计算为错误
		if nil == err || !strings.HasPrefix(err.Error(), ErrPrefixRequestCancel) {
			p.doPostErrFilters(c)
		}

		result.Err = err
		result.Code = resCode
		return
	}

	log.Infof("Backend server[%s] responsed, code <%d>, body<%s>", svr.Addr, res.StatusCode(), res.Body())

	// post filters
	filterName, code, err = p.doPostFilters(c)
	if nil != err {
		log.InfoErrorf(err, "Proxy Filter-Post<%s> fail: %s ", filterName, err.Error())

		result.Err = err
		result.Code = code
		return
	}
}

func (p *Proxy) writeResult(ctx *fasthttp.RequestCtx, res *fasthttp.Response) {
	ctx.SetStatusCode(res.StatusCode())
	ctx.Write(res.Body())
}
