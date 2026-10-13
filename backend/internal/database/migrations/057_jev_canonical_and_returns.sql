ALTER TABLE jev_arb_positions
    ADD COLUMN IF NOT EXISTS long_canonical_symbol TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS long_quote_asset TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS short_canonical_symbol TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS short_quote_asset TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS entry_mid_basis_bps NUMERIC NULL;

UPDATE jev_arb_positions p
SET
    long_quote_asset = COALESCE(NULLIF(p.long_quote_asset, ''), upper(i.quote_asset), upper(p.quote_asset), ''),
    long_canonical_symbol = CASE
        WHEN p.long_canonical_symbol <> '' THEN p.long_canonical_symbol
        WHEN i.base_asset IS NOT NULL AND i.quote_asset IS NOT NULL
            THEN upper(i.base_asset) || upper(i.quote_asset)
        WHEN p.base_asset <> '' AND p.quote_asset <> ''
            THEN upper(p.base_asset) || upper(p.quote_asset)
        ELSE p.long_canonical_symbol
    END
FROM instruments i
WHERE lower(i.exchange) = lower(p.long_exchange)
  AND upper(i.exchange_symbol) = upper(p.long_symbol);

UPDATE jev_arb_positions p
SET
    short_quote_asset = COALESCE(NULLIF(p.short_quote_asset, ''), upper(i.quote_asset), upper(p.quote_asset), ''),
    short_canonical_symbol = CASE
        WHEN p.short_canonical_symbol <> '' THEN p.short_canonical_symbol
        WHEN i.base_asset IS NOT NULL AND i.quote_asset IS NOT NULL
            THEN upper(i.base_asset) || upper(i.quote_asset)
        WHEN p.base_asset <> '' AND p.quote_asset <> ''
            THEN upper(p.base_asset) || upper(p.quote_asset)
        ELSE p.short_canonical_symbol
    END
FROM instruments i
WHERE lower(i.exchange) = lower(p.short_exchange)
  AND upper(i.exchange_symbol) = upper(p.short_symbol);

ALTER TABLE jev_arb_decisions
    ADD COLUMN IF NOT EXISTS base_asset TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS quote_asset TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS long_canonical_symbol TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS long_quote_asset TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS short_canonical_symbol TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS short_quote_asset TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS funding_return_24h NUMERIC NULL,
    ADD COLUMN IF NOT EXISTS holding_horizon_net_return NUMERIC NULL,
    ADD COLUMN IF NOT EXISTS expected_payback_minutes NUMERIC NULL,
    ADD COLUMN IF NOT EXISTS current_mid_basis_bps NUMERIC NULL,
    ADD COLUMN IF NOT EXISTS entry_mid_basis_bps NUMERIC NULL,
    ADD COLUMN IF NOT EXISTS basis_change_since_entry_bps NUMERIC NULL,
    ADD COLUMN IF NOT EXISTS expected_basis_return_24h NUMERIC NULL,
    ADD COLUMN IF NOT EXISTS basis_return_p10 NUMERIC NULL,
    ADD COLUMN IF NOT EXISTS basis_profit_probability NUMERIC NULL,
    ADD COLUMN IF NOT EXISTS basis_forecast_confidence NUMERIC NULL,
    ADD COLUMN IF NOT EXISTS basis_sample_count INTEGER NULL,
    ADD COLUMN IF NOT EXISTS basis_model_version TEXT NULL,
    ADD COLUMN IF NOT EXISTS basis_forecast_calculated_at TIMESTAMPTZ NULL;

UPDATE jev_arb_decisions d
SET
    base_asset = COALESCE(NULLIF(d.base_asset, ''), p.base_asset, ''),
    quote_asset = COALESCE(NULLIF(d.quote_asset, ''), p.quote_asset, ''),
    long_canonical_symbol = COALESCE(NULLIF(d.long_canonical_symbol, ''), p.long_canonical_symbol, ''),
    long_quote_asset = COALESCE(NULLIF(d.long_quote_asset, ''), p.long_quote_asset, ''),
    short_canonical_symbol = COALESCE(NULLIF(d.short_canonical_symbol, ''), p.short_canonical_symbol, ''),
    short_quote_asset = COALESCE(NULLIF(d.short_quote_asset, ''), p.short_quote_asset, '')
FROM jev_arb_positions p
WHERE d.position_id = p.id;
