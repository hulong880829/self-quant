#!/usr/bin/env python3
"""
One-shot fixture generator for Hyperliquid signing compatibility.

Uses the official Hyperliquid Python SDK to produce canonical
action_hash and signature values for a fixed set of L1 and
user-signed actions. The Go fixture-based tests compare against
these values byte-for-byte.

Run once and commit testfixtures/signing_fixtures.json. Re-run
only when intentionally extending or updating fixtures.
"""

import json
import sys
from pathlib import Path

import msgpack
from eth_account import Account
from hyperliquid.utils import signing


# Deterministic test private key. Matches the existing signing_test.go fixture key.
TEST_PK_HEX = "0xabcd1234567890abcd1234567890abcd1234567890abcd1234567890abcd1234"
TEST_TIMESTAMP = 1703001234567
TEST_NONCE = 1703001234567

# Pre-derived from TEST_PK_HEX (account.address) — captured at generation time.
account = Account.from_key(TEST_PK_HEX)


def sig_to_obj(sig):
    """Normalize signing.sign_inner result into a JSON-friendly dict."""
    return {"r": sig["r"], "s": sig["s"], "v": int(sig["v"])}


def gen_l1(action, vault, nonce, expires_after, is_mainnet):
    msgpack_bytes = msgpack.packb(action)
    hash_bytes = signing.action_hash(action, vault, nonce, expires_after)
    sig = signing.sign_l1_action(account, action, vault, nonce, expires_after, is_mainnet)
    return {
        "msgpack_hex": msgpack_bytes.hex(),
        "action_hash_hex": hash_bytes.hex(),
        "signature": sig_to_obj(sig),
    }


def gen_user_signed(action, payload_types, primary_type, is_mainnet):
    # sign_user_signed_action mutates `action` by adding signatureChainId and hyperliquidChain.
    # We capture the *final* action that gets serialized so the Go side can replicate it.
    a = dict(action)
    sig = signing.sign_user_signed_action(account, a, payload_types, primary_type, is_mainnet)
    return {
        "primary_type": primary_type,
        "payload_types": payload_types,
        "final_action": a,
        "signature": sig_to_obj(sig),
    }


def main():
    fixtures = {
        "test_private_key": TEST_PK_HEX,
        "test_address": account.address,
        "l1_actions": {},
        "user_signed_actions": {},
    }

    # -------------------- L1 actions --------------------
    # Plain order action — currently expected to pass on Go side.
    fixtures["l1_actions"]["plain_order_testnet"] = {
        "inputs": {
            "action": {
                "type": "order",
                "orders": [{
                    "a": 0,
                    "b": True,
                    "p": "100.0",
                    "s": "1.0",
                    "r": False,
                    "t": {"limit": {"tif": "Gtc"}},
                }],
                "grouping": "na",
            },
            "vault": None,
            "nonce": TEST_NONCE,
            "expires_after": None,
            "is_mainnet": False,
        },
        "expected": gen_l1(
            {
                "type": "order",
                "orders": [{
                    "a": 0, "b": True, "p": "100.0", "s": "1.0", "r": False,
                    "t": {"limit": {"tif": "Gtc"}},
                }],
                "grouping": "na",
            },
            None, TEST_NONCE, None, False,
        ),
    }

    # Builder-fee order — the smoking-gun case. The `builder` address
    # is a 42-char string, which crosses msgpack's str8/str16 threshold.
    builder_order_action = {
        "type": "order",
        "orders": [{
            "a": 0, "b": True, "p": "100.0", "s": "1.0", "r": False,
            "t": {"limit": {"tif": "Gtc"}},
        }],
        "grouping": "na",
        "builder": {"b": "0x1234567890abcdef1234567890abcdef12345678", "f": 50},
    }
    fixtures["l1_actions"]["builder_fee_order_testnet"] = {
        "inputs": {
            "action": builder_order_action,
            "vault": None,
            "nonce": TEST_NONCE,
            "expires_after": None,
            "is_mainnet": False,
        },
        "expected": gen_l1(dict(builder_order_action), None, TEST_NONCE, None, False),
    }
    fixtures["l1_actions"]["builder_fee_order_mainnet"] = {
        "inputs": {
            "action": builder_order_action,
            "vault": None,
            "nonce": TEST_NONCE,
            "expires_after": None,
            "is_mainnet": True,
        },
        "expected": gen_l1(dict(builder_order_action), None, TEST_NONCE, None, True),
    }

    # Builder-fee order with vault
    fixtures["l1_actions"]["builder_fee_order_with_vault"] = {
        "inputs": {
            "action": builder_order_action,
            "vault": "0xabcdef0123456789abcdef0123456789abcdef01",
            "nonce": TEST_NONCE,
            "expires_after": None,
            "is_mainnet": False,
        },
        "expected": gen_l1(
            dict(builder_order_action),
            "0xabcdef0123456789abcdef0123456789abcdef01",
            TEST_NONCE, None, False,
        ),
    }

    # updateLeverage (small action, sanity)
    fixtures["l1_actions"]["update_leverage"] = {
        "inputs": {
            "action": {"type": "updateLeverage", "asset": 0, "isCross": True, "leverage": 10},
            "vault": None,
            "nonce": TEST_NONCE,
            "expires_after": None,
            "is_mainnet": False,
        },
        "expected": gen_l1(
            {"type": "updateLeverage", "asset": 0, "isCross": True, "leverage": 10},
            None, TEST_NONCE, None, False,
        ),
    }

    # updateIsolatedMargin (currently goes through Python — we want it pure Go)
    fixtures["l1_actions"]["update_isolated_margin"] = {
        "inputs": {
            "action": {"type": "updateIsolatedMargin", "asset": 0, "isBuy": True, "ntli": 1000000},
            "vault": None,
            "nonce": TEST_NONCE,
            "expires_after": None,
            "is_mainnet": False,
        },
        "expected": gen_l1(
            {"type": "updateIsolatedMargin", "asset": 0, "isBuy": True, "ntli": 1000000},
            None, TEST_NONCE, None, False,
        ),
    }

    # -------------------- User-signed actions --------------------
    # Withdraw from bridge
    fixtures["user_signed_actions"]["withdraw_from_bridge"] = {
        "inputs": {
            "action": {
                "type": "withdraw3",
                "destination": "0x1234567890abcdef1234567890abcdef12345678",
                "amount": "100.0",
                "time": TEST_TIMESTAMP,
            },
            "is_mainnet": False,
        },
        "expected": gen_user_signed(
            {
                "type": "withdraw3",
                "destination": "0x1234567890abcdef1234567890abcdef12345678",
                "amount": "100.0",
                "time": TEST_TIMESTAMP,
            },
            signing.WITHDRAW_SIGN_TYPES,
            "HyperliquidTransaction:Withdraw",
            False,
        ),
    }

    # usdClassTransfer
    fixtures["user_signed_actions"]["usd_class_transfer"] = {
        "inputs": {
            "action": {
                "type": "usdClassTransfer",
                "amount": "100.0",
                "toPerp": True,
                "nonce": TEST_NONCE,
            },
            "is_mainnet": False,
        },
        "expected": gen_user_signed(
            {
                "type": "usdClassTransfer",
                "amount": "100.0",
                "toPerp": True,
                "nonce": TEST_NONCE,
            },
            signing.USD_CLASS_TRANSFER_SIGN_TYPES,
            "HyperliquidTransaction:UsdClassTransfer",
            False,
        ),
    }

    # approveBuilderFee
    fixtures["user_signed_actions"]["approve_builder_fee"] = {
        "inputs": {
            "action": {
                "type": "approveBuilderFee",
                "maxFeeRate": "0.1%",
                "builder": "0x1234567890abcdef1234567890abcdef12345678",
                "nonce": TEST_NONCE,
            },
            "is_mainnet": False,
        },
        "expected": gen_user_signed(
            {
                "type": "approveBuilderFee",
                "maxFeeRate": "0.1%",
                "builder": "0x1234567890abcdef1234567890abcdef12345678",
                "nonce": TEST_NONCE,
            },
            [
                {"name": "hyperliquidChain", "type": "string"},
                {"name": "maxFeeRate", "type": "string"},
                {"name": "builder", "type": "address"},
                {"name": "nonce", "type": "uint64"},
            ],
            "HyperliquidTransaction:ApproveBuilderFee",
            False,
        ),
    }

    # approveAgent
    fixtures["user_signed_actions"]["approve_agent"] = {
        "inputs": {
            "action": {
                "type": "approveAgent",
                "agentAddress": "0x1234567890abcdef1234567890abcdef12345678",
                "agentName": "test-agent",
                "nonce": TEST_NONCE,
            },
            "is_mainnet": False,
        },
        "expected": gen_user_signed(
            {
                "type": "approveAgent",
                "agentAddress": "0x1234567890abcdef1234567890abcdef12345678",
                "agentName": "test-agent",
                "nonce": TEST_NONCE,
            },
            [
                {"name": "hyperliquidChain", "type": "string"},
                {"name": "agentAddress", "type": "address"},
                {"name": "agentName", "type": "string"},
                {"name": "nonce", "type": "uint64"},
            ],
            "HyperliquidTransaction:ApproveAgent",
            False,
        ),
    }

    # usdSend
    fixtures["user_signed_actions"]["usd_send"] = {
        "inputs": {
            "action": {
                "type": "usdSend",
                "destination": "0x1234567890abcdef1234567890abcdef12345678",
                "amount": "50.0",
                "time": TEST_TIMESTAMP,
            },
            "is_mainnet": False,
        },
        "expected": gen_user_signed(
            {
                "type": "usdSend",
                "destination": "0x1234567890abcdef1234567890abcdef12345678",
                "amount": "50.0",
                "time": TEST_TIMESTAMP,
            },
            signing.USD_SEND_SIGN_TYPES,
            "HyperliquidTransaction:UsdSend",
            False,
        ),
    }

    # tokenDelegate
    fixtures["user_signed_actions"]["token_delegate"] = {
        "inputs": {
            "action": {
                "type": "tokenDelegate",
                "validator": "0x1234567890abcdef1234567890abcdef12345678",
                "wei": 1000000,
                "isUndelegate": False,
                "nonce": TEST_NONCE,
            },
            "is_mainnet": False,
        },
        "expected": gen_user_signed(
            {
                "type": "tokenDelegate",
                "validator": "0x1234567890abcdef1234567890abcdef12345678",
                "wei": 1000000,
                "isUndelegate": False,
                "nonce": TEST_NONCE,
            },
            signing.TOKEN_DELEGATE_TYPES,
            "HyperliquidTransaction:TokenDelegate",
            False,
        ),
    }

    # convertToMultiSigUser
    fixtures["user_signed_actions"]["convert_to_multi_sig_user"] = {
        "inputs": {
            "action": {
                "type": "convertToMultiSigUser",
                "signers": '{"authorizedUsers":["0x1234567890abcdef1234567890abcdef12345678"],"threshold":1}',
                "nonce": TEST_NONCE,
            },
            "is_mainnet": False,
        },
        "expected": gen_user_signed(
            {
                "type": "convertToMultiSigUser",
                "signers": '{"authorizedUsers":["0x1234567890abcdef1234567890abcdef12345678"],"threshold":1}',
                "nonce": TEST_NONCE,
            },
            signing.CONVERT_TO_MULTI_SIG_USER_SIGN_TYPES,
            "HyperliquidTransaction:ConvertToMultiSigUser",
            False,
        ),
    }

    # spotSend
    fixtures["user_signed_actions"]["spot_send"] = {
        "inputs": {
            "action": {
                "type": "spotSend",
                "destination": "0x1234567890abcdef1234567890abcdef12345678",
                "token": "PURR/USDC",
                "amount": "5.0",
                "time": TEST_TIMESTAMP,
            },
            "is_mainnet": False,
        },
        "expected": gen_user_signed(
            {
                "type": "spotSend",
                "destination": "0x1234567890abcdef1234567890abcdef12345678",
                "token": "PURR/USDC",
                "amount": "5.0",
                "time": TEST_TIMESTAMP,
            },
            signing.SPOT_TRANSFER_SIGN_TYPES,
            "HyperliquidTransaction:SpotSend",
            False,
        ),
    }

    out_path = Path(__file__).parent / "signing_fixtures.json"
    out_path.write_text(json.dumps(fixtures, indent=2, sort_keys=False))
    print(f"Wrote {out_path}", file=sys.stderr)


if __name__ == "__main__":
    main()
