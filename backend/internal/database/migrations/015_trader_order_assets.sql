UPDATE trader_orders AS orders
SET base_asset = instruments.base_asset,
    quote_asset = instruments.quote_asset
FROM instruments
WHERE orders.instrument_id = instruments.id
  AND (orders.base_asset = '' OR orders.quote_asset = '');
