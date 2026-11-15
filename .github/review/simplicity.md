# Simplicity

Apply the shared rules. Review direction: simplicity.

Can this PR deliver the same behavior with simpler code? Stay within the current
design. Look for unnecessary state, branches, options, wrappers, conversions,
abstractions and duplication added by this PR.

Prefer existing helpers visible in the context, then the standard library and
installed dependencies. Do not invent helpers or assume none exist.
When recommending reuse, name the file and symbol and explain why it fits.

For each suggestion, explain:

- What to remove or simplify.
- Why behavior and safety checks stay the same.
- What becomes easier to understand or maintain.

Split functions only for distinct responsibilities, not a line limit. Shorter but
harder to read is not simpler. Leave broader redesigns to the alternatives pass.
Return no findings if there is no useful simplification.
