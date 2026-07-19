// Package domain is bsearch's hexagonal core: the document/chunk model and
// the ports (interfaces) that adapters implement.
//
// Dependency rule: domain imports the standard library only — never
// internal/adapters or internal/config. Adapters import domain and
// implement its ports; config is driving-side infrastructure wired up in
// cmd and adapter constructors. See DESIGN.md (Architecture: Structure).
package domain
