from .star_pkg import star_helper


def use_star() -> int:
    # The package __init__ star-imports this name, so the direct import lookup
    # misses and the cold-run retry recovers it from its import provenance.
    return star_helper(1)
