package trampoline

import (
	"bytes"
	"math"
	"math/big"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TestValidatePayloadRoles checks the exact required and allowed field sets
// for every trampoline onion hop role.
func TestValidatePayloadRoles(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		role  HopRole
		valid []tlv.Type
	}{
		{
			name: "clear intermediate",
			role: RoleClearIntermediate,
			valid: []tlv.Type{
				record.AmtOnionType,
				record.LockTimeOnionType,
				OutgoingNodeIDType,
			},
		},
		{
			name: "bolt 11 final",
			role: RoleBolt11Final,
			valid: []tlv.Type{
				record.AmtOnionType,
				record.LockTimeOnionType,
				record.MPPOnionType,
				record.MetadataOnionType,
			},
		},
		{
			name: "recipient paths",
			role: RoleRecipientPaths,
			valid: []tlv.Type{
				record.AmtOnionType,
				record.LockTimeOnionType,
				RecipientFeaturesType,
				RecipientBlindedPathsType,
			},
		},
		{
			name: "blinded introduction",
			role: RoleBlindedIntroduction,
			valid: []tlv.Type{
				record.EncryptedDataOnionType,
				record.BlindingPointOnionType,
			},
		},
		{
			name: "blinded intermediate",
			role: RoleBlindedIntermediate,
			valid: []tlv.Type{
				record.EncryptedDataOnionType,
			},
		},
		{
			name: "blinded final",
			role: RoleBlindedFinal,
			valid: []tlv.Type{
				record.AmtOnionType,
				record.LockTimeOnionType,
				record.EncryptedDataOnionType,
				record.TotalAmtMsatBlindedType,
			},
		},
		{
			name: "one hop blinded path",
			role: RoleBlindedIntroductionFinal,
			valid: []tlv.Type{
				record.AmtOnionType,
				record.LockTimeOnionType,
				record.EncryptedDataOnionType,
				record.BlindingPointOnionType,
				record.TotalAmtMsatBlindedType,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			valid := typeMap(test.valid...)
			require.NoError(t, ValidatePayload(valid, test.role))

			rules, err := rulesForRole(test.role)
			require.NoError(t, err)
			for _, required := range rules.required {
				missing := cloneTypeMap(valid)
				delete(missing, required)

				err := ValidatePayload(missing, test.role)
				require.Equal(t, ErrInvalidPayload{
					Role:      test.role,
					Type:      required,
					Violation: OmittedViolation,
				}, err)
			}
		})
	}
}

// TestValidatePayloadRejectsKnownFields verifies deterministic rejection of a
// known but role-inappropriate field and strict blinded unknown fields.
func TestValidatePayloadRejectsKnownFields(t *testing.T) {
	t.Parallel()

	clearTypes := typeMap(
		record.AmtOnionType,
		record.LockTimeOnionType,
		OutgoingNodeIDType,
		RecipientFeaturesType,
	)
	err := ValidatePayload(clearTypes, RoleClearIntermediate)
	require.Equal(t, ErrInvalidPayload{
		Role:      RoleClearIntermediate,
		Type:      RecipientFeaturesType,
		Violation: IncludedViolation,
	}, err)

	blinded := typeMap(record.EncryptedDataOnionType, 23, 25)
	err = ValidatePayload(blinded, RoleBlindedIntermediate)
	require.Equal(t, ErrInvalidPayload{
		Role:      RoleBlindedIntermediate,
		Type:      23,
		Violation: IncludedViolation,
	}, err)

	clearTypes = typeMap(
		record.AmtOnionType,
		record.LockTimeOnionType,
		OutgoingNodeIDType,
		23,
	)
	require.NoError(t, ValidatePayload(clearTypes, RoleClearIntermediate))
}

// TestValidateFinalOuter checks the final recipient's amount and expiry
// comparisons for both single-part and multi-part outer payments.
func TestValidateFinalOuter(t *testing.T) {
	t.Parallel()

	amount := uint64(100)
	cltv := uint32(500)
	inner := &Payload{
		AmountToForward: &amount,
		OutgoingCLTV:    &cltv,
	}
	total := uint64(100)

	tests := []struct {
		name  string
		outer OuterPayment
		err   error
	}{
		{
			name: "single part exact",
			outer: OuterPayment{
				AmountMsat: 100,
				CLTV:       500,
			},
		},
		{
			name: "multi part exact",
			outer: OuterPayment{
				AmountMsat:      50,
				CLTV:            500,
				TotalAmountMsat: &total,
			},
		},
		{
			name: "cltv short",
			outer: OuterPayment{
				AmountMsat: 100,
				CLTV:       499,
			},
			err: ErrOuterCLTVInsufficient,
		},
		{
			name: "amount short",
			outer: OuterPayment{
				AmountMsat: 99,
				CLTV:       500,
			},
			err: ErrOuterAmountInsufficient,
		},
		{
			name: "total short",
			outer: OuterPayment{
				AmountMsat:      100,
				CLTV:            500,
				TotalAmountMsat: ptr(uint64(99)),
			},
			err: ErrOuterTotalInsufficient,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateFinalOuter(test.outer, inner)
			require.ErrorIs(t, err, test.err)
		})
	}
}

// TestSelectPathKey checks the exclusive inner-or-outer path-key rule.
func TestSelectPathKey(t *testing.T) {
	t.Parallel()

	innerKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{1}, 32))
	outerKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{2}, 32))

	_, err := SelectPathKey(nil, nil)
	require.ErrorIs(t, err, ErrPathKeyMissing)

	_, err = SelectPathKey(innerKey.PubKey(), outerKey.PubKey())
	require.ErrorIs(t, err, ErrPathKeyDuplicated)

	selected, err := SelectPathKey(innerKey.PubKey(), nil)
	require.NoError(t, err)
	require.True(t, selected.IsEqual(innerKey.PubKey()))

	selected, err = SelectPathKey(nil, outerKey.PubKey())
	require.NoError(t, err)
	require.True(t, selected.IsEqual(outerKey.PubKey()))
}

// TestRelayBudgetBoundaries checks exact budgets and arithmetic overflow.
func TestRelayBudgetBoundaries(t *testing.T) {
	t.Parallel()

	amount := uint64(100_000_000)
	cltv := uint32(800_000)
	inner := &Payload{
		AmountToForward: &amount,
		OutgoingCLTV:    &cltv,
	}

	requiredAmount, requiredCLTV, err := RequiredRelayBudget(
		inner, 500, 1000, 36,
	)
	require.NoError(t, err)
	require.Equal(t, uint64(100_100_500), requiredAmount)
	require.Equal(t, uint32(800_036), requiredCLTV)

	require.NoError(t, ValidateRelayBudget(
		OuterPayment{
			AmountMsat: requiredAmount,
			CLTV:       requiredCLTV,
		},
		inner, 500, 1000, 36,
	))
	require.ErrorIs(t, ValidateRelayBudget(
		OuterPayment{
			AmountMsat: requiredAmount - 1,
			CLTV:       requiredCLTV,
		},
		inner, 500, 1000, 36,
	), ErrOuterAmountInsufficient)
	require.ErrorIs(t, ValidateRelayBudget(
		OuterPayment{
			AmountMsat: requiredAmount,
			CLTV:       requiredCLTV - 1,
		},
		inner, 500, 1000, 36,
	), ErrOuterCLTVInsufficient)

	overflowAmount := uint64(math.MaxUint64)
	inner.AmountToForward = &overflowAmount
	_, _, err = RequiredRelayBudget(inner, 1, 0, 0)
	require.ErrorIs(t, err, ErrFeeOverflow)

	overflowCLTV := uint32(math.MaxUint32)
	inner.AmountToForward = &amount
	inner.OutgoingCLTV = &overflowCLTV
	_, _, err = RequiredRelayBudget(inner, 0, 0, 1)
	require.ErrorIs(t, err, ErrCLTVOverflow)
}

// TestCalculateForwardingAmount checks blinded-relay fee reversal at exact
// boundaries, including the full uint64 amount domain.
func TestCalculateForwardingAmount(t *testing.T) {
	t.Parallel()

	amount, err := CalculateForwardingAmount(1_100, 100, 0)
	require.NoError(t, err)
	require.Equal(t, uint64(1_000), amount)

	_, err = CalculateForwardingAmount(99, 100, 0)
	require.ErrorIs(t, err, ErrOuterAmountInsufficient)

	amount, err = CalculateForwardingAmount(
		math.MaxUint64, math.MaxUint32, math.MaxUint32,
	)
	require.NoError(t, err)
	require.Less(t, amount, uint64(math.MaxUint64))
}

// TestCalculateForwardingAmountProperties compares the overflow-safe
// implementation with the same ceiling equation evaluated using big integers.
func TestCalculateForwardingAmountProperties(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		incoming := rapid.Uint64().Draw(rt, "incoming")
		base := rapid.Uint32().Draw(rt, "base")
		ppm := rapid.Uint32().Draw(rt, "ppm")
		if incoming < uint64(base) {
			_, err := CalculateForwardingAmount(incoming, base, ppm)
			require.ErrorIs(rt, err, ErrOuterAmountInsufficient)
			return
		}

		actual, err := CalculateForwardingAmount(incoming, base, ppm)
		require.NoError(rt, err)

		numerator := new(big.Int).SetUint64(incoming - uint64(base))
		numerator.Mul(numerator, big.NewInt(1_000_000))
		denominator := new(big.Int).SetUint64(
			1_000_000 + uint64(ppm),
		)
		numerator.Add(numerator, new(big.Int).Sub(
			new(big.Int).Set(denominator), big.NewInt(1),
		))
		expected := numerator.Div(numerator, denominator).Uint64()
		require.Equal(rt, expected, actual)
		require.LessOrEqual(rt, actual, incoming)
	})
}

// TestRelayBudgetProperties checks amount conservation, non-negative fees,
// and monotonic CLTV reduction across generated relay policies.
func TestRelayBudgetProperties(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		amount := rapid.Uint64Range(
			0, math.MaxUint64/1_000_000,
		).Draw(rt, "amount_msat")
		base := rapid.Uint32().Draw(rt, "base_msat")
		ppm := rapid.Uint32().Draw(rt, "ppm")
		cltv := rapid.Uint32Range(
			0, math.MaxUint32-math.MaxUint16,
		).Draw(rt, "cltv")
		delta := rapid.Uint16().Draw(rt, "delta")

		inner := &Payload{
			AmountToForward: &amount,
			OutgoingCLTV:    &cltv,
		}
		requiredAmount, requiredCLTV, err := RequiredRelayBudget(
			inner, base, ppm, delta,
		)
		require.NoError(rt, err)
		require.GreaterOrEqual(rt, requiredAmount, amount)
		require.GreaterOrEqual(rt, requiredCLTV, cltv)

		fee, err := CalculateFee(amount, base, ppm)
		require.NoError(rt, err)
		require.Equal(rt, amount+fee, requiredAmount)
		require.Equal(rt, cltv+uint32(delta), requiredCLTV)

		require.NoError(rt, ValidateRelayBudget(
			OuterPayment{
				AmountMsat: requiredAmount,
				CLTV:       requiredCLTV,
			},
			inner, base, ppm, delta,
		))
	})
}

// TestValidationDeterministic checks that map insertion order never changes
// the field selected for a strict-role violation.
func TestValidationDeterministic(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		reverse := rapid.Bool().Draw(rt, "reverse")
		parsed := typeMap(record.EncryptedDataOnionType)
		if reverse {
			parsed[25] = nil
			parsed[23] = nil
		} else {
			parsed[23] = nil
			parsed[25] = nil
		}

		err := ValidatePayload(parsed, RoleBlindedIntermediate)
		require.Equal(rt, ErrInvalidPayload{
			Role:      RoleBlindedIntermediate,
			Type:      23,
			Violation: IncludedViolation,
		}, err)
	})
}

// TestValidationNames verifies stable diagnostics for every role, violation,
// and structured payload error, including unknown future values.
func TestValidationNames(t *testing.T) {
	t.Parallel()

	roles := []HopRole{
		RoleClearIntermediate,
		RoleBolt11Final,
		RoleRecipientPaths,
		RoleBlindedIntroduction,
		RoleBlindedIntermediate,
		RoleBlindedFinal,
		RoleBlindedIntroductionFinal,
	}
	for _, role := range roles {
		require.NotContains(t, role.String(), "unknown")
	}
	require.Contains(t, HopRole(255).String(), "unknown-role")
	require.Equal(t, "omitted", OmittedViolation.String())
	require.Equal(t, "included", IncludedViolation.String())
	require.Contains(
		t, PayloadViolation(255).String(), "unknown-violation",
	)
	require.Equal(
		t,
		"trampoline payload for bolt11-final omitted type 8",
		(ErrInvalidPayload{
			Role: RoleBolt11Final, Type: 8,
			Violation: OmittedViolation,
		}).Error(),
	)
}

func typeMap(types ...tlv.Type) tlv.TypeMap {
	result := make(tlv.TypeMap, len(types))
	for _, typ := range types {
		result[typ] = nil
	}

	return result
}

func cloneTypeMap(source tlv.TypeMap) tlv.TypeMap {
	result := make(tlv.TypeMap, len(source))
	for typ, value := range source {
		result[typ] = value
	}

	return result
}

func ptr[T any](value T) *T {
	return &value
}
