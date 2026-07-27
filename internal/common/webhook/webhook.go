// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// NoopDeleteValidator supplies the ValidateDelete half of
// admission.Validator[T] to webhooks that guard create and update only. Embed
// it in a webhook struct to inherit the method.
//
// The method is required by the Validator interface but is never invoked: the
// validating webhooks register only create/update (no delete verb), so with
// failurePolicy=Fail a down operator can never block CR — and thereby
// namespace — deletion.
type NoopDeleteValidator[T runtime.Object] struct{}

// ValidateDelete implements the delete half of admission.Validator[T] by
// allowing every deletion. See the NoopDeleteValidator doc comment for why it
// is never reached in the first place.
func (NoopDeleteValidator[T]) ValidateDelete(context.Context, T) (admission.Warnings, error) {
	return nil, nil
}
