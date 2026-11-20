from .star_pkg import star_helper


def use_star() -> int:
    # The package __init__ star-imports this name, so the export lookup follows
    # one wildcard hop instead of leaving it for the pending retry (#102).
    return star_helper(1)
