// Package androidwacontacts is the Android WhatsApp contacts parser of plan 4I: android.whatsapp.contacts, which
// reads the wa_contacts table of wa.db for WhatsApp and WhatsApp Business. It is a pure parser: it reads only the
// files the host hands it. One contact record per person row (spec 6.6); every other row, a JID only, a nameless row
// or an address that is no person, is one counted warning (R2). It holds no time of its own for a contact.
package androidwacontacts
