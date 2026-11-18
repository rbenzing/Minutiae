// Package ioswacalls is the iOS WhatsApp calls parser of plan 4I2: ios.whatsapp.calls, which reads CallHistory.sqlite (call
// events of a Core Data store) for WhatsApp and WhatsApp Business. It is a pure parser: it reads only the files the host hands
// it, imports no other parser and holds no state.
//
// A call takes its direction only from an aggregate (ZWAAGGREGATECALLEVENT) that links exactly this one call; the counterparty
// is never stated (the participant table is not joined). The package also holds the test fixtures and their oracles
// (tools/fixtures/whatsapp_ios_calls.py).
package ioswacalls
