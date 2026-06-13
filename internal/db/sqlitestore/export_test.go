package sqlitestore

// LinksChangedPayloadForTest exposes linksChangedPayload to external tests so
// they can pin the exact wire bytes without making the function public.
var LinksChangedPayloadForTest = linksChangedPayload
