ALTER TABLE market_data.crypto_bbo
  ADD COLUMN IF NOT EXISTS instrument_id UInt64 DEFAULT 0,
  ADD COLUMN IF NOT EXISTS venue_symbol String DEFAULT '';
