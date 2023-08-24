package runtime

import (
	"context"
	"fmt"
	"strings"

	"encoding/json"
	"net/http"

	"github.com/graphql-go/graphql"
	"google.golang.org/grpc"
)

type (
	// MiddlewareFunc type definition
	MiddlewareFunc func(ctx context.Context, w http.ResponseWriter, r *http.Request) (context.Context, error)

	// GraphQLMiddlewareFunc type definition
	GraphQLMiddlewareFunc func(ctx context.Context, r *http.Request, method string) error
)

type GraphqlHandler interface {
	CreateConnection(context.Context) (*grpc.ClientConn, func(), error)
	GetMutations(*grpc.ClientConn) graphql.Fields
	GetQueries(*grpc.ClientConn) graphql.Fields
}

// ServeMux is struct can execute graphql request via incoming HTTP request.
// This is inspired from grpc-gateway implementation, thanks!
type ServeMux struct {
	middlewares        []MiddlewareFunc
	graphQLMiddlewares map[string]map[string]GraphQLMiddlewareFunc
	ErrorHandler       GraphqlErrorHandler

	handlers []GraphqlHandler
}

// NewServeMux creates ServeMux pointer
func NewServeMux(ms ...MiddlewareFunc) *ServeMux {
	return &ServeMux{
		middlewares:        ms,
		handlers:           make([]GraphqlHandler, 0),
		graphQLMiddlewares: make(map[string]map[string]GraphQLMiddlewareFunc),
	}
}

// AddHandler registers graphql handler which is built via plugin
func (s *ServeMux) AddHandler(h GraphqlHandler) error {
	if err := s.validateHandler(h); err != nil {
		return err
	}
	s.handlers = append(s.handlers, h)
	return nil
}

// Validate handler definition
func (s *ServeMux) validateHandler(h GraphqlHandler) error {
	queries := h.GetQueries(nil)
	mutations := h.GetMutations(nil)

	// If handler doesn't have any definitions, pass
	if len(queries) == 0 && len(mutations) == 0 {
		return nil
	}

	schemaConfig := graphql.SchemaConfig{}
	if len(queries) > 0 {
		schemaConfig.Query = graphql.NewObject(graphql.ObjectConfig{
			Name:   "Query",
			Fields: queries,
		})
	}
	if len(mutations) > 0 {
		schemaConfig.Mutation = graphql.NewObject(graphql.ObjectConfig{
			Name:   "Mutation",
			Fields: mutations,
		})
	}

	// Try to generate Schema and check error
	if _, err := graphql.NewSchema(schemaConfig); err != nil {
		return fmt.Errorf("Schema validation error: %s", err)
	}
	return nil
}

// Use adds more middlwares
func (s *ServeMux) Use(ms ...MiddlewareFunc) *ServeMux {
	s.middlewares = append(s.middlewares, ms...)
	return s
}

// Use adds more middlwares
func (s *ServeMux) UseDirective(method string, directive string, ms GraphQLMiddlewareFunc) *ServeMux {
	directives, ok := s.graphQLMiddlewares[method]
	if !ok {
		directives = make(map[string]GraphQLMiddlewareFunc)
	}

	directives[directive] = ms
	s.graphQLMiddlewares[method] = directives
	return s
}

// ServeHTTP implements http.Handler
func (s *ServeMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	for _, m := range s.middlewares {
		var err error
		ctx, err = m(ctx, w, r)
		if err != nil {
			ge := GraphqlError{}
			if me, ok := err.(*MiddlewareError); ok {
				ge.Message = me.Message
				ge.Extensions = map[string]interface{}{
					"code": me.Code,
				}
			} else {
				ge.Message = err.Error()
				ge.Extensions = map[string]interface{}{
					"code": "MIDDLEWARE_ERROR",
				}
			}
			respondResult(w, &graphql.Result{
				Errors: []GraphqlError{ge},
			})
			return
		}
	}

	queries := graphql.Fields{}
	mutations := graphql.Fields{}
	for _, h := range s.handlers {
		c, closer, err := h.CreateConnection(ctx)
		if err != nil {
			respondResult(w, &graphql.Result{
				Errors: []GraphqlError{
					{
						Message: "Failed to create grpc connection: " + err.Error(),
						Extensions: map[string]interface{}{
							"code": "GRPC_CONNECT_ERROR",
						},
					},
				},
			})
			return
		}
		defer closer()

		for k, v := range h.GetQueries(c) {
			queries[k] = v
		}
		for k, v := range h.GetMutations(c) {
			mutations[k] = v
		}
	}

	schemaConfig := graphql.SchemaConfig{}
	if len(queries) > 0 {
		schemaConfig.Query = graphql.NewObject(graphql.ObjectConfig{
			Name:   "Query",
			Fields: queries,
		})
	}
	if len(mutations) > 0 {
		schemaConfig.Mutation = graphql.NewObject(graphql.ObjectConfig{
			Name:   "Mutation",
			Fields: mutations,
		})
	}

	schema, err := graphql.NewSchema(schemaConfig)
	if err != nil {
		respondResult(w, &graphql.Result{
			Errors: []GraphqlError{
				{
					Message: "Failed to build schema: " + err.Error(),
					Extensions: map[string]interface{}{
						"code": "SCHEMA_GENERATION_ERROR",
					},
				},
			},
		})
		return
	}

	req, err := parseRequest(r)
	if err != nil {
		respondResult(w, &graphql.Result{
			Errors: []GraphqlError{
				{
					Message: "Failed to parse request: " + err.Error(),
					Extensions: map[string]interface{}{
						"code": "REQUEST_PARSE_ERROR",
					},
				},
			},
		})
		return
	}

	methodNames := make([]string, 0, len(queries)+len(mutations))
	for method := range queries {
		methodNames = append(methodNames, method)
	}
	for method := range mutations {
		methodNames = append(methodNames, method)
	}

	for _, method := range methodNames {
		if !strings.Contains(req.Query, method) {
			continue
		}
		if directives, ok := s.graphQLMiddlewares[method]; ok {
			for _, fn := range directives {
				if err = fn(ctx, r, method); err != nil {
					ge := GraphqlError{}
					if me, ok := err.(*MiddlewareError); ok {
						ge.Message = me.Message
						ge.Extensions = map[string]interface{}{
							"code": me.Code,
						}
					} else {
						ge.Message = err.Error()
						ge.Extensions = map[string]interface{}{
							"code": "DIRECTIVE_MIDDLEWARE_ERROR",
						}
					}
					respondResult(w, &graphql.Result{
						Errors: []GraphqlError{ge},
					})
					return
				}
			}
		}
	}

	result := graphql.Do(graphql.Params{
		Schema:         schema,
		RequestString:  req.Query,
		VariableValues: req.Variables,
		Context:        ctx,
	})

	if len(result.Errors) > 0 {
		if s.ErrorHandler != nil {
			s.ErrorHandler(result.Errors)
		} else {
			defaultGraphqlErrorHandler(result.Errors)
		}
	}
	respondResult(w, result)
}

func respondResult(w http.ResponseWriter, result *graphql.Result) {
	out, _ := json.Marshal(result) // nolint: errcheck

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprint(len(out)))
	w.WriteHeader(http.StatusOK)
	w.Write(out) // nolint: errcheck
}
