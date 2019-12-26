package predictor

import (
	"context"
	"github.com/go-logr/logr"
	guuid "github.com/google/uuid"
	"github.com/seldonio/seldon-core/executor/api/client"
	"github.com/seldonio/seldon-core/executor/api/payload"
	payloadLogger "github.com/seldonio/seldon-core/executor/logger"
	"github.com/seldonio/seldon-core/operator/apis/machinelearning/v1"
	"net/url"
	"sync"
)

type PredictorProcess struct {
	Ctx       context.Context
	Client    client.SeldonApiClient
	Log       logr.Logger
	RequestId string
	ServerUrl *url.URL
	Namespace string
}

func NewPredictorProcess(context context.Context, client client.SeldonApiClient, log logr.Logger, requestId string, serverUrl *url.URL, namespace string) PredictorProcess {
	if requestId == "" {
		requestId = guuid.New().String()
	}
	return PredictorProcess{
		Ctx:       context,
		Client:    client,
		Log:       log,
		RequestId: requestId,
		ServerUrl: serverUrl,
		Namespace: namespace,
	}
}

func hasMethod(method v1.PredictiveUnitMethod, methods *[]v1.PredictiveUnitMethod) bool {
	if methods != nil {
		for _, m := range *methods {
			if m == method {
				return true
			}
		}
	}
	return false
}

func (p *PredictorProcess) transformInput(node *v1.PredictiveUnit, msg payload.SeldonPayload) (payload.SeldonPayload, error) {
	if (*node).Type != nil {
		switch *node.Type {
		case v1.MODEL:
			msg, err := p.Client.Chain(p.Ctx, msg)
			if err != nil {
				return nil, err
			}
			return p.Client.Predict(p.Ctx, node.Name, node.Endpoint.ServiceHost, node.Endpoint.ServicePort, msg)
		case v1.TRANSFORMER:
			msg, err := p.Client.Chain(p.Ctx, msg)
			if err != nil {
				return nil, err
			}
			return p.Client.TransformInput(p.Ctx, node.Name, node.Endpoint.ServiceHost, node.Endpoint.ServicePort, msg)
		}
	}
	if hasMethod(v1.TRANSFORM_INPUT, node.Methods) {
		msg, err := p.Client.Chain(p.Ctx, msg)
		if err != nil {
			return nil, err
		}
		return p.Client.TransformInput(p.Ctx, node.Name, node.Endpoint.ServiceHost, node.Endpoint.ServicePort, msg)
	}
	return msg, nil
}

func (p *PredictorProcess) transformOutput(node *v1.PredictiveUnit, msg payload.SeldonPayload) (payload.SeldonPayload, error) {
	if (*node).Type != nil {
		switch *node.Type {
		case v1.OUTPUT_TRANSFORMER:
			msg, err := p.Client.Chain(p.Ctx, msg)
			if err != nil {
				return nil, err
			}
			return p.Client.TransformOutput(p.Ctx, node.Name, node.Endpoint.ServiceHost, node.Endpoint.ServicePort, msg)
		}
	}
	if hasMethod(v1.TRANSFORM_OUTPUT, node.Methods) {
		return p.Client.TransformOutput(p.Ctx, node.Name, node.Endpoint.ServiceHost, node.Endpoint.ServicePort, msg)
	}
	return msg, nil
}

func (p *PredictorProcess) route(node *v1.PredictiveUnit, msg payload.SeldonPayload) (int, error) {
	if (*node).Type != nil {
		switch *node.Type {
		case v1.ROUTER:
			return p.Client.Route(p.Ctx, node.Name, node.Endpoint.ServiceHost, node.Endpoint.ServicePort, msg)
		}
	}
	if hasMethod(v1.ROUTE, node.Methods) {
		return p.Client.Route(p.Ctx, node.Name, node.Endpoint.ServiceHost, node.Endpoint.ServicePort, msg)
	}
	if node.Implementation != nil && *node.Implementation == v1.RANDOM_ABTEST {
		return p.abTestRouter(node)
	}
	return -1, nil
}

func (p *PredictorProcess) aggregate(node *v1.PredictiveUnit, msg []payload.SeldonPayload) (payload.SeldonPayload, error) {
	if (*node).Type != nil {
		switch *node.Type {
		case v1.COMBINER:
			return p.Client.Combine(p.Ctx, node.Name, node.Endpoint.ServiceHost, node.Endpoint.ServicePort, msg)
		}
	}
	if hasMethod(v1.AGGREGATE, node.Methods) {
		return p.Client.Combine(p.Ctx, node.Name, node.Endpoint.ServiceHost, node.Endpoint.ServicePort, msg)
	}
	return msg[0], nil
}

func (p *PredictorProcess) routeChildren(node *v1.PredictiveUnit, msg payload.SeldonPayload) (payload.SeldonPayload, error) {
	if node.Children != nil && len(node.Children) > 0 {
		route, err := p.route(node, msg)
		if err != nil {
			return nil, err
		}
		var cmsgs []payload.SeldonPayload
		if route == -1 {
			cmsgs = make([]payload.SeldonPayload, len(node.Children))
			var errs = make([]error, len(node.Children))
			wg := sync.WaitGroup{}
			for i, nodeChild := range node.Children {
				wg.Add(1)
				go func(i int, nodeChild v1.PredictiveUnit, msg payload.SeldonPayload) {
					cmsgs[i], errs[i] = p.Execute(&nodeChild, msg)
					wg.Done()
				}(i, nodeChild, msg)
			}
			wg.Wait()
			for i, err := range errs {
				if err != nil {
					return cmsgs[i], err
				}
			}
		} else {
			cmsgs = make([]payload.SeldonPayload, 1)
			cmsgs[0], err = p.Execute(&node.Children[route], msg)
			if err != nil {
				return cmsgs[0], err
			}
		}
		return p.aggregate(node, cmsgs)
	} else {
		return msg, nil
	}
}

func (p *PredictorProcess) getLogUrl(logger *v1.Logger) (*url.URL, error) {
	if logger.Url != nil {
		return url.Parse(*logger.Url)
	} else {
		return url.Parse(payloadLogger.GetLoggerDefaultUrl(p.Namespace))
	}
}

func (p *PredictorProcess) logPayload(nodeName string, logger *v1.Logger, reqType payloadLogger.LogRequestType, msg payload.SeldonPayload) error {
	payload, err := msg.GetBytes()
	if err != nil {
		return err
	}
	logUrl, err := p.getLogUrl(logger)
	if err != nil {
		return err
	}

	payloadLogger.QueueLogRequest(payloadLogger.LogRequest{
		Url:         logUrl,
		Bytes:       &payload,
		ContentType: msg.GetContentType(),
		ReqType:     reqType,
		Id:          p.RequestId,
		SourceUri:   p.ServerUrl,
		ModelId:     nodeName,
	})
	return nil
}

func (p *PredictorProcess) Execute(node *v1.PredictiveUnit, msg payload.SeldonPayload) (payload.SeldonPayload, error) {
	//Log Request
	if node.Logger != nil && (node.Logger.Mode == v1.LogRequest || node.Logger.Mode == v1.LogAll) {
		p.logPayload(node.Name, node.Logger, payloadLogger.InferenceRequest, msg)
	}
	tmsg, err := p.transformInput(node, msg)
	if err != nil {
		return tmsg, err
	}
	cmsg, err := p.routeChildren(node, tmsg)
	if err != nil {
		return tmsg, err
	}
	response, err := p.transformOutput(node, cmsg)
	// Log Response
	if err == nil && node.Logger != nil && (node.Logger.Mode == v1.LogResponse || node.Logger.Mode == v1.LogAll) {
		p.logPayload(node.Name, node.Logger, payloadLogger.InferenceResponse, msg)
	}
	return response, err
}