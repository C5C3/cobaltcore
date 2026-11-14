// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package identity

// Sentinels lists every sentinel error callers branch on. Tests assert that a
// failure outside these classes matches none of them.
var Sentinels = []error{ErrNotFound, ErrConflict, ErrUnauthorized, ErrForbidden}

// NewHTTPClientWithRequestTimeout builds a client with a shorter per-request
// timeout; it is the only way a test shortens RequestTimeout.
var NewHTTPClientWithRequestTimeout = newHTTPClient
