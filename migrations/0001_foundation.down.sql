-- 0001 foundation: drop the shared trigger function and schema orvex (LBT-10 spec P-5, P-11)

DROP FUNCTION orvex.forbid_mutation();

-- RESTRICT (the default): fails if anything else remains in the schema.
DROP SCHEMA orvex;
