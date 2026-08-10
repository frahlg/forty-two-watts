package notifications

// The push sentence table is generated from contract/push-catalogue.yaml —
// the app's file, paired byte for byte like contract/registry.yaml. The app
// owns all prose; push is the stated exception where the box must render
// text, and this is the only place it may take that text from.

//go:generate go run ../appproto/gencontract/cmd push ../../../contract/push-catalogue.yaml catalogue_gen.go
