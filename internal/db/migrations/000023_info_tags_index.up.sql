-- Every result submission looks up its info row by tags. Without an index the
-- lookup reads the whole table, which grows with every distinct info. A hash
-- index serves the equality lookup at any tag size; it is not unique because
-- concurrent submissions may already have stored duplicate tags.
CREATE INDEX info_tags_index ON info USING hash (tags);
