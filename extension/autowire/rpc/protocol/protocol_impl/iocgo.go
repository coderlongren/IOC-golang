package protocol_impl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"time"

	"dubbo.apache.org/dubbo-go/v3/protocol/invocation"
	"github.com/gin-gonic/gin"

	"github.com/alibaba/ioc-golang/autowire/util"

	"github.com/opentracing/opentracing-go"

	tracer2 "github.com/alibaba/ioc-golang/debug/interceptor/trace"

	"github.com/alibaba/ioc-golang/debug/interceptor/trace"

	"github.com/fatih/color"

	"dubbo.apache.org/dubbo-go/v3/common/constant"
	dubboProtocol "dubbo.apache.org/dubbo-go/v3/protocol"

	"github.com/alibaba/ioc-golang/common"
	"github.com/alibaba/ioc-golang/extension/autowire/rpc/protocol"
)

// +ioc:autowire=true
// +ioc:autowire:type=normal
// +ioc:autowire:paramType=Param
// +ioc:autowire:constructFunc=Init

// IOCProtocol is ioc protocol impl
type IOCProtocol struct {
	address    string
	exportPort string
	timeout    string
}

func (i *IOCProtocol) Invoke(invocation dubboProtocol.Invocation) dubboProtocol.Result {
	sdID, _ := invocation.GetAttachment("sdid")
	data, _ := json.Marshal(invocation.Arguments())
	invokeURL := DefaultSchema + "://" + i.address + "/" + sdID + "/" + invocation.MethodName()

	timeoutDuration, err := time.ParseDuration(i.timeout)
	if err != nil {
		timeoutDuration = time.Second * 3
	}
	requestCtx, cancel := context.WithTimeout(context.Background(), timeoutDuration)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, invokeURL, bytes.NewBuffer(data))
	if err != nil {
		return &dubboProtocol.RPCResult{
			Err: err,
		}
	}

	// inject tracing context if necessary
	if currentSpan := tracer2.GetTraceInterceptor().GetCurrentSpan(); currentSpan != nil {
		// current rpc invocation is in tracing link
		carrier := opentracing.HTTPHeadersCarrier(req.Header)
		_ = trace.GetGlobalTracer().Inject(currentSpan.Context(), opentracing.HTTPHeaders, carrier)
	}

	rsp, err := http.DefaultClient.Do(req)
	if err != nil {
		color.Red("[IOC Protocol] Invoke %s with error = %s", invokeURL, err)
		return &dubboProtocol.RPCResult{
			Err: err,
		}
	}
	rspData, _ := ioutil.ReadAll(rsp.Body)
	replyList := invocation.Reply().(*[]interface{})
	finalIsError := false
	finalErrorNotNil := false
	if length := len(*replyList); length > 0 {
		_, ok := (*replyList)[length-1].(*error)
		if ok {
			finalIsError = true
		}
	}
	err = json.Unmarshal(rspData, replyList)
	if err != nil && finalIsError {
		// error message must be returned
		finalErrorNotNil = true

		// calculate error message detail, try to recover unmarshal failed caused by error not empty, first try to unmarshal to string
		(*replyList)[len(*replyList)-1] = ""
		err = json.Unmarshal(rspData, replyList)
		if err != nil {
			// error is not nil, means previous unmarshal failed because of invalid response, write error message
			err = fmt.Errorf("[IOC Protocol] Unmarshal response from %s with error %s, response data details is %s", invokeURL, err, string(rspData))
			(*replyList)[len(*replyList)-1] = err
		}
		// error is nil means final return value error is returned from server side, and the response is valid
	}
	if err != nil {
		return &dubboProtocol.RPCResult{
			Err: err,
		}
	}
	if finalErrorNotNil {
		realErr := fmt.Errorf((*replyList)[len(*replyList)-1].(string))
		(*replyList)[len(*replyList)-1] = &realErr
	}
	return nil
}

func (i *IOCProtocol) Export(invoker dubboProtocol.Invoker) dubboProtocol.Exporter {
	httpServer := getSingletonGinEngion(i.exportPort)

	sdid := invoker.GetURL().GetParam(constant.InterfaceKey, "")
	clientStubFullName := invoker.GetURL().GetParam(common.AliasKey, "")
	svc := ServiceMap.GetServiceByServiceKey(IOCProtocolName, sdid)
	if svc == nil {
		return nil
	}

	for methodName, methodType := range svc.Method() {
		argsType := methodType.ArgsType()
		tempMethod := methodName
		httpServer.POST(fmt.Sprintf("/%s/%s", clientStubFullName, tempMethod), func(c *gin.Context) {
			reqData, err := ioutil.ReadAll(c.Request.Body)
			if err != nil {
				c.AbortWithStatusJSON(http.StatusInternalServerError, err.Error())
				return
			}
			arguments, err := ParseArgs(argsType, reqData)
			if err != nil {
				c.AbortWithStatusJSON(http.StatusInternalServerError, err.Error())
				return
			}

			carrier := opentracing.HTTPHeadersCarrier(c.Request.Header)
			clientContext, err := trace.GetGlobalTracer().Extract(opentracing.HTTPHeaders, carrier)
			if err == nil {
				traceCtx := &trace.Context{
					SDID:              util.ToRPCServiceSDID(clientStubFullName),
					MethodName:        tempMethod,
					ClientSpanContext: clientContext,
				}
				trace.GetTraceInterceptor().TraceThisGR(traceCtx)
				defer trace.GetTraceInterceptor().UnTrace(traceCtx)
			}
			rsp := invoker.Invoke(context.Background(),
				invocation.NewRPCInvocation(tempMethod, arguments, nil)).Result()
			c.PureJSON(http.StatusOK, rsp)
		})
	}

	return dubboProtocol.NewBaseExporter(sdid, invoker, nil)
}

func getSingletonGinEngion(exportPort string) *gin.Engine {
	if ginEngionSingleton == nil {
		ginEngionSingleton = gin.Default()
		go func() {
			if err := ginEngionSingleton.Run(":" + exportPort); err != nil {
				// FIXME, should throw error gracefully
				panic(err)
			}
		}()
	}
	return ginEngionSingleton
}

var ginEngionSingleton *gin.Engine

var _ protocol.Protocol = &IOCProtocol{}
