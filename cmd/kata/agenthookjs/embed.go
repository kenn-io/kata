// Package agenthookjs contains reviewed JavaScript composed into native adapters.
package agenthookjs

import _ "embed"

// PlanCore is the shared Kata plan core embedded into native adapters.
//
//go:embed plan_core.js
var PlanCore string

// PlanOmp is the OMP integration layer embedded into native adapters.
//
//go:embed plan_omp.js
var PlanOmp string
