package descriptor

import (
	"fmt"
	"strings"

	"github.com/gengo/grpc-gateway/protoc-gen-grpc-gateway/httprule"
	options "github.com/gengo/grpc-gateway/third_party/googleapis/google/api"
	"github.com/golang/glog"
	"github.com/golang/protobuf/proto"
	descriptor "github.com/golang/protobuf/protoc-gen-go/descriptor"
)

// loadServices registers services and their methods from "targetFile" to "r".
// It must be called after loadFile is called for all files so that loadServices
// can resolve names of message types and their fields.
func (r *Registry) loadServices(targetFile string) error {
	file := r.files[targetFile]
	if file == nil {
		return fmt.Errorf("no such file: %s", targetFile)
	}
	var svcs []*Service
	for _, sd := range file.GetService() {
		svc := &Service{
			File: file,
			ServiceDescriptorProto: sd,
		}
		for _, md := range sd.GetMethod() {
			opts, err := extractAPIOptions(md)
			if err != nil {
				glog.Errorf("Failed to extract ApiMethodOptions from %s.%s: %v", svc.GetName(), md.GetName(), err)
				return err
			}
			if opts == nil {
				glog.V(1).Infof("Skip non-target method: %s.%s", svc.GetName(), md.GetName())
				continue
			}
			meth, err := r.newMethod(svc, md, opts)
			if err != nil {
				return err
			}
			svc.Methods = append(svc.Methods, meth)
		}
		if len(svc.Methods) == 0 {
			continue
		}
		svcs = append(svcs, svc)
	}
	file.Services = svcs
	return nil
}

func (r *Registry) newMethod(svc *Service, md *descriptor.MethodDescriptorProto, opts *options.HttpRule) (*Method, error) {
	var (
		httpMethod   string
		pathTemplate string
	)
	switch {
	case opts.Get != "":
		httpMethod = "GET"
		pathTemplate = opts.Get
		if opts.Body != "" {
			return nil, fmt.Errorf("needs request body even though http method is GET: %s", md.GetName())
		}

	case opts.Put != "":
		httpMethod = "PUT"
		pathTemplate = opts.Put

	case opts.Post != "":
		httpMethod = "POST"
		pathTemplate = opts.Post

	case opts.Delete != "":
		httpMethod = "DELETE"
		pathTemplate = opts.Delete
		if opts.Body != "" {
			return nil, fmt.Errorf("needs request body even though http method is DELETE: %s", md.GetName())
		}

	case opts.Patch != "":
		httpMethod = "PATCH"
		pathTemplate = opts.Patch

	case opts.Custom != nil:
		httpMethod = opts.Custom.Kind
		pathTemplate = opts.Custom.Path

	default:
		glog.Errorf("No pattern specified in google.api.HttpRule: %s", md.GetName())
		return nil, fmt.Errorf("none of pattern specified")
	}

	parsed, err := httprule.Parse(pathTemplate)
	if err != nil {
		return nil, err
	}
	tmpl := parsed.Compile()

	if md.GetClientStreaming() && len(tmpl.Fields) > 0 {
		return nil, fmt.Errorf("cannot use path parameter in client streaming")
	}

	requestType, err := r.LookupMsg(svc.File.GetPackage(), md.GetInputType())
	if err != nil {
		return nil, err
	}
	responseType, err := r.LookupMsg(svc.File.GetPackage(), md.GetOutputType())
	if err != nil {
		return nil, err
	}

	meth := &Method{
		Service:               svc,
		MethodDescriptorProto: md,
		PathTmpl:              tmpl,
		HTTPMethod:            httpMethod,
		RequestType:           requestType,
		ResponseType:          responseType,
	}

	for _, f := range tmpl.Fields {
		param, err := r.newParam(meth, f)
		if err != nil {
			return nil, err
		}
		meth.PathParams = append(meth.PathParams, param)
	}

	// TODO(yugui) Handle query params

	meth.Body, err = r.newBody(meth, opts.Body)
	if err != nil {
		return nil, err
	}

	return meth, nil
}

func extractAPIOptions(meth *descriptor.MethodDescriptorProto) (*options.HttpRule, error) {
	if meth.Options == nil {
		return nil, nil
	}
	if !proto.HasExtension(meth.Options, options.E_Http) {
		return nil, nil
	}
	ext, err := proto.GetExtension(meth.Options, options.E_Http)
	if err != nil {
		return nil, err
	}
	opts, ok := ext.(*options.HttpRule)
	if !ok {
		return nil, fmt.Errorf("extension is %T; want an HttpRule", ext)
	}
	return opts, nil
}

func (r *Registry) newParam(meth *Method, path string) (Parameter, error) {
	msg := meth.RequestType
	fields, err := r.resolveFiledPath(msg, path)
	if err != nil {
		return Parameter{}, err
	}
	l := len(fields)
	if l == 0 {
		return Parameter{}, fmt.Errorf("invalid field access list for %s", path)
	}
	target := fields[l-1].Target
	switch target.GetType() {
	case descriptor.FieldDescriptorProto_TYPE_MESSAGE, descriptor.FieldDescriptorProto_TYPE_GROUP:
		return Parameter{}, fmt.Errorf("aggregate type %s in parameter of %s.%s: %s", target.Type, meth.Service.GetName(), meth.GetName(), path)
	}
	return Parameter{
		FieldPath: FieldPath(fields),
		Method:    meth,
		Target:    fields[l-1].Target,
	}, nil
}

func (r *Registry) newBody(meth *Method, path string) (*Body, error) {
	msg := meth.RequestType
	switch path {
	case "":
		return nil, nil
	case "*":
		return &Body{
			DecoderFactoryExpr: "json.NewDecoder",
			DecoderImports: []GoPackage{
				{
					Path: "encoding/json",
					Name: "json",
				},
			},
			FieldPath: nil,
		}, nil
	}
	fields, err := r.resolveFiledPath(msg, path)
	if err != nil {
		return nil, err
	}
	return &Body{
		DecoderFactoryExpr: "json.NewDecoder",
		DecoderImports: []GoPackage{
			{
				Path: "encoding/json",
				Name: "json",
			},
		},
		FieldPath: FieldPath(fields),
	}, nil
}

// lookupField looks up a field named "name" within "msg".
// It returns nil if no such field found.
func lookupField(msg *Message, name string) *Field {
	for _, f := range msg.Fields {
		if f.GetName() == name {
			return f
		}
	}
	return nil
}

// resolveFieldPath resolves "path" into a list of fieldDescriptor, starting from "msg".
func (r *Registry) resolveFiledPath(msg *Message, path string) ([]FieldPathComponent, error) {
	if path == "" {
		return nil, nil
	}

	root := msg
	var result []FieldPathComponent
	for i, c := range strings.Split(path, ".") {
		if i > 0 {
			f := result[i-1].Target
			switch f.GetType() {
			case descriptor.FieldDescriptorProto_TYPE_MESSAGE, descriptor.FieldDescriptorProto_TYPE_GROUP:
				var err error
				msg, err = r.LookupMsg(msg.FQMN(), f.GetTypeName())
				if err != nil {
					return nil, err
				}
			default:
				return nil, fmt.Errorf("not an aggregate type: %s in %s", f.GetName(), path)
			}
		}

		glog.Infof("Lookup %s in %s", c, msg.FQMN())
		f := lookupField(msg, c)
		if f == nil {
			return nil, fmt.Errorf("no field %q found in %s", path, root.GetName())
		}
		result = append(result, FieldPathComponent{Name: c, Target: f})
	}
	return result, nil
}