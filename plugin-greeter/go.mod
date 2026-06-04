module github.com/jacobsee/upstream-readiness/plugin-greeter

go 1.25.0

require (
	github.com/jacobsee/upstream-readiness/pluginkit v0.0.0-00010101000000-000000000000
	github.com/jacobsee/upstream-readiness/proto v0.0.0
	google.golang.org/grpc v1.81.1
)

require (
	golang.org/x/net v0.56.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260226221140-a57be14db171 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)

replace github.com/jacobsee/upstream-readiness/proto => ../proto

replace github.com/jacobsee/upstream-readiness/pluginkit => ../pluginkit
