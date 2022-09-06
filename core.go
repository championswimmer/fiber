package fiber

import (
	"fmt"
	"net"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/gofiber/fiber/v3/utils"
	"github.com/valyala/fasthttp"
)

// Route is a struct that holds all metadata for each registered handler
type Route struct {
	// Data for routing
	pos         uint32      // Position in stack -> important for the sort of the matched routes
	use         bool        // USE matches path prefixes
	star        bool        // Path equals '*'
	root        bool        // Path equals '/'
	path        string      // Prettified path
	routeParser routeParser // Parameter parser

	// Public fields
	Method   string    `json:"method"` // HTTP method
	Path     string    `json:"path"`   // Original registered route path
	Params   []string  `json:"params"` // Case sensitive param keys
	Handlers []Handler `json:"-"`      // Ctx handlers
}

type disableLogger struct{}

func (dl *disableLogger) Printf(_ string, _ ...interface{}) {
	// fmt.Println(fmt.Sprintf(format, args...))
}

func (app *App) init() *App {
	// lock application
	app.mutex.Lock()

	// Only load templates if a view engine is specified
	if app.config.Views != nil {
		if err := app.config.Views.Load(); err != nil {
			fmt.Printf("views: %v\n", err)
		}
	}

	// create fasthttp server
	app.server = &fasthttp.Server{
		Logger:       &disableLogger{},
		LogAllErrors: false,
		ErrorHandler: app.serverErrorHandler,
	}

	// fasthttp server settings
	app.server.Handler = app.handler
	app.server.Name = app.config.ServerHeader
	app.server.Concurrency = app.config.Concurrency
	app.server.NoDefaultDate = app.config.DisableDefaultDate
	app.server.NoDefaultContentType = app.config.DisableDefaultContentType
	app.server.DisableHeaderNamesNormalizing = app.config.DisableHeaderNormalizing
	app.server.DisableKeepalive = app.config.DisableKeepalive
	app.server.MaxRequestBodySize = app.config.BodyLimit
	app.server.NoDefaultServerHeader = app.config.ServerHeader == ""
	app.server.ReadTimeout = app.config.ReadTimeout
	app.server.WriteTimeout = app.config.WriteTimeout
	app.server.IdleTimeout = app.config.IdleTimeout
	app.server.ReadBufferSize = app.config.ReadBufferSize
	app.server.WriteBufferSize = app.config.WriteBufferSize
	app.server.GetOnly = app.config.GETOnly
	app.server.ReduceMemoryUsage = app.config.ReduceMemoryUsage
	app.server.StreamRequestBody = app.config.StreamRequestBody
	app.server.DisablePreParseMultipartForm = app.config.DisablePreParseMultipartForm

	// unlock application
	app.mutex.Unlock()
	return app
}

// Mount attaches another app instance as a sub-router along a routing path.
// It's very useful to split up a large API as many independent routers and
// compose them as a single service using Mount. The fiber's error handler and
// any of the fiber's sub apps are added to the application's error handlers
// to be invoked on errors that happen within the prefix route.
func (app *App) mount(prefix string, sub *App) IRouter {
	prefix = strings.TrimRight(prefix, "/")
	if prefix == "" {
		prefix = "/"
	}

	if app.parent == nil {
		sub.parent = app
		sub.path = app.mountpath + prefix
		sub.mountpath = prefix
		app.subList[app.mountpath+prefix] = sub
	}

	sub.parent = app
	sub.path = app.mountpath + prefix
	sub.mountpath = prefix
	sub.subList[app.mountpath+prefix] = sub

	atomic.AddUint32(&app.handlersCount, sub.handlersCount)

	return app
}

// serverErrorHandler is a wrapper around the application's error handler method
// user for the fasthttp server configuration. It maps a set of fasthttp errors to fiber
// errors before calling the application's error handler method.
func (app *App) serverErrorHandler(fctx *fasthttp.RequestCtx, err error) {
	c := app.AcquireCtx(fctx)
	if _, ok := err.(*fasthttp.ErrSmallBuffer); ok {
		err = ErrRequestHeaderFieldsTooLarge
	} else if netErr, ok := err.(*net.OpError); ok && netErr.Timeout() {
		err = ErrRequestTimeout
	} else if err == fasthttp.ErrBodyTooLarge {
		err = ErrRequestEntityTooLarge
	} else if err == fasthttp.ErrGetOnly {
		err = ErrMethodNotAllowed
	} else if strings.Contains(err.Error(), "timeout") {
		err = ErrRequestTimeout
	} else {
		err = ErrBadRequest
	}

	if catch := app.ErrorHandler(c, err); catch != nil {
		_ = c.SendStatus(StatusInternalServerError)
	}

	app.ReleaseCtx(c)
}

func (app *App) registerRouter(prefix string, router *Router) {
	app.routerList[prefix] = router
}

func (r *Route) match(detectionPath, path string, params *[maxParams]string) (match bool) {
	// root detectionPath check
	if r.root && detectionPath == "/" {
		return true
		// '*' wildcard matches any detectionPath
	} else if r.star {
		if len(path) > 1 {
			params[0] = path[1:]
		} else {
			params[0] = ""
		}
		return true
	}
	// Does this route have parameters
	if len(r.Params) > 0 {
		// Match params
		if match := r.routeParser.getMatch(detectionPath, path, params, r.use); match {
			// Get params from the path detectionPath
			return match
		}
	}
	// Is this route a Middleware?
	if r.use {
		// Single slash will match or detectionPath prefix
		if r.root || strings.HasPrefix(detectionPath, r.path) {
			return true
		}
		// Check for a simple detectionPath match
	} else if len(r.path) == len(detectionPath) && r.path == detectionPath {
		return true
	}
	// No match
	return false
}

func (app *App) next(c *Ctx) (match bool, err error) {
	// Get stack length
	tree, ok := app.treeStack[c.methodINT][c.treePath]
	if !ok {
		tree = app.treeStack[c.methodINT][""]
	}
	lenr := len(tree) - 1

	// Loop over the route stack starting from previous index
	for c.indexRoute < lenr {
		// Increment route index
		c.indexRoute++

		// Get *Route
		route := tree[c.indexRoute]

		// Check if it matches the request path
		match = route.match(c.detectionPath, c.path, &c.values)

		// No match, next route
		if !match {
			continue
		}
		// Pass route reference and param values
		c.route = route

		// Non use handler matched
		if !c.matched && !route.use {
			c.matched = true
		}

		// Execute first handler of route
		c.indexHandler = 0
		err = route.Handlers[0](c)
		return match, err // Stop scanning the stack
	}

	// If c.Next() does not match, return 404
	err = NewError(StatusNotFound, "Cannot "+c.method+" "+c.pathOriginal)

	// If no match, scan stack again if other methods match the request
	// Moved from app.handler because middleware may break the route chain
	if !c.matched && methodExist(c) {
		err = ErrMethodNotAllowed
	}
	return
}

func (app *App) handler(rctx *fasthttp.RequestCtx) {
	// Acquire Ctx with fasthttp request from pool
	c := app.AcquireCtx(rctx)

	// handle invalid http method directly
	if c.methodINT == -1 {
		_ = c.Status(StatusBadRequest).SendString("Invalid http method")
		app.ReleaseCtx(c)
		return
	}

	// Find match in stack
	_, err := app.next(c)
	if err != nil {
		if catch := c.app.ErrorHandler(c, err); catch != nil {
			_ = c.SendStatus(StatusInternalServerError)
		}
	}

	// Release Ctx
	app.ReleaseCtx(c)
}

func (app *App) addPrefixToRoute(prefix string, route *Route) *Route {
	prefixedPath := getGroupPath(prefix, route.Path)
	prettyPath := prefixedPath
	// Case sensitive routing, all to lowercase
	if !app.config.CaseSensitive {
		prettyPath = utils.ToLower(prettyPath)
	}
	// Strict routing, remove trailing slashes
	if !app.config.Strict && len(prettyPath) > 1 {
		prettyPath = utils.TrimRight(prettyPath, '/')
	}

	route.Path = prefixedPath
	route.path = RemoveEscapeChar(prettyPath)
	route.routeParser = parseRoute(prettyPath)
	route.root = false
	route.star = false

	return route
}

func (app *App) copyRoute(route *Route) *Route {
	return &Route{
		// Router booleans
		use:  route.use,
		star: route.star,
		root: route.root,

		// Path data
		path:        route.path,
		routeParser: route.routeParser,
		Params:      route.Params,

		// Public data
		Path:     route.Path,
		Method:   route.Method,
		Handlers: route.Handlers,
	}
}

func (app *App) register(method, pathRaw string, handlers ...Handler) IRouter {
	// Uppercase HTTP methods
	method = utils.ToUpper(method)
	// Check if the HTTP method is valid unless it's USE
	if method != methodUse && methodInt(method) == -1 {
		panic(fmt.Sprintf("add: invalid http method %s\n", method))
	}
	// A route requires atleast one ctx handler
	if len(handlers) == 0 {
		panic(fmt.Sprintf("missing handler in route: %s\n", pathRaw))
	}
	// Cannot have an empty path
	if pathRaw == "" {
		pathRaw = "/"
	}
	// Path always start with a '/'
	if pathRaw[0] != '/' {
		pathRaw = "/" + pathRaw
	}
	// Create a stripped path in-case sensitive / trailing slashes
	pathPretty := pathRaw
	// Case sensitive routing, all to lowercase
	if !app.config.CaseSensitive {
		pathPretty = utils.ToLower(pathPretty)
	}
	// Strict routing, remove trailing slashes
	if !app.config.Strict && len(pathPretty) > 1 {
		pathPretty = utils.TrimRight(pathPretty, '/')
	}
	// Is layer a middleware?
	isUse := method == methodUse
	// Is path a direct wildcard?
	isStar := pathPretty == "/*"
	// Is path a root slash?
	isRoot := pathPretty == "/"
	// Parse path parameters
	parsedRaw := parseRoute(pathRaw)
	parsedPretty := parseRoute(pathPretty)

	// Create route metadata without pointer
	route := Route{
		// Router booleans
		use:  isUse,
		star: isStar,
		root: isRoot,

		// Path data
		path:        RemoveEscapeChar(pathPretty),
		routeParser: parsedPretty,
		Params:      parsedRaw.params,

		// Public data
		Path:     pathRaw,
		Method:   method,
		Handlers: handlers,
	}
	// Increment global handler count
	atomic.AddUint32(&app.handlersCount, uint32(len(handlers)))

	// Middleware route matches all HTTP methods
	if isUse {
		// Add route to all HTTP methods stack
		for _, m := range intMethod {
			// Create a route copy to avoid duplicates during compression
			r := route
			app.addRoute(m, &r)
		}
	} else {
		// Add route to stack
		app.addRoute(method, &route)
	}
	return app
}

func (app *App) addRoute(method string, route *Route) {
	// Get unique HTTP method identifier
	m := methodInt(method)

	// prevent identically route registration
	l := len(app.stack[m])
	if l > 0 && app.stack[m][l-1].Path == route.Path && route.use == app.stack[m][l-1].use {
		preRoute := app.stack[m][l-1]
		preRoute.Handlers = append(preRoute.Handlers, route.Handlers...)
	} else {
		// Increment global route position
		route.pos = atomic.AddUint32(&app.routesCount, 1)
		route.Method = method
		// Add route to the stack
		app.stack[m] = append(app.stack[m], route)
		app.routesRefreshed = true
	}
}

// buildTree build the prefix tree from the previously registered routes
func (app *App) buildTree() *App {
	// build prefix tree from the previously registered sub app's routes
	for _, sub := range app.subList {
		stack := sub.stack
		for m := range stack {
			for r := range stack[m] {
				route := app.copyRoute(stack[m][r])
				sub.parent.addRoute(route.Method, app.addPrefixToRoute(sub.path, route))

			}
		}
	}

	// build prefix tree from the previously registered router's routes
	for path, rtr := range app.routerList {
		stack := rtr.stack
		for m := range stack {
			for r := range stack[m] {
				route := app.copyRoute(stack[m][r])
				app.addRoute(route.Method, app.addPrefixToRoute(path, route))
				atomic.AddUint32(&app.handlersCount, uint32(len(route.Handlers)))
			}
		}
	}

	if !app.routesRefreshed {
		return app
	}

	// loop all the methods and stacks and create the prefix tree
	for m := range intMethod {
		tsMap := make(map[string][]*Route)
		for _, route := range app.stack[m] {
			treePath := ""
			if len(route.routeParser.segs) > 0 && len(route.routeParser.segs[0].Const) >= 3 {
				treePath = route.routeParser.segs[0].Const[:3]
			}
			// create tree stack
			tsMap[treePath] = append(tsMap[treePath], route)
		}
		app.treeStack[m] = tsMap
	}

	// loop the methods and tree stacks and add global stack and sort everything
	for m := range intMethod {
		tsMap := app.treeStack[m]
		for treePart := range tsMap {
			if treePart != "" {
				// merge global tree routes in current tree stack
				tsMap[treePart] = uniqueRouteStack(append(tsMap[treePart], tsMap[""]...))
			}
			// sort tree slices with the positions
			slc := tsMap[treePart]
			sort.Slice(slc, func(i, j int) bool { return slc[i].pos < slc[j].pos })
		}
	}

	app.routesRefreshed = false

	return app
}

// startupProcess Is the method which executes all the necessary processes just before the start of the server.
func (app *App) startupProcess() *App {
	app.mutex.Lock()
	app.buildTree()
	app.mutex.Unlock()
	return app
}
