package apiprotocol

// MCPCapabilities describes registered contracts admitted for this caller.
// Resource authority and provider availability are checked again per call.
type MCPCapabilities struct {
	Version   int                  `json:"version"`
	Delegated bool                 `json:"delegated"`
	Routes    []MCPRouteDescriptor `json:"routes"`
}

type MCPRouteDescriptor struct {
	OperationID       string   `json:"operation_id"`
	Method            string   `json:"method"`
	Path              string   `json:"path"`
	QueryParameters   []string `json:"query_parameters"`
	RequestProperties []string `json:"request_properties"`
}
