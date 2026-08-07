package apiauth

// The role table is generated from the same registry as everything else the
// box shares with the app. It lives here rather than beside the wire
// constants because the enrolment record, the HTTP layer and the app protocol
// all need it, and this is the one package all three can import.

//go:generate go run ../appproto/gencontract/cmd roles ../../../contract/registry.yaml contract_gen.go
