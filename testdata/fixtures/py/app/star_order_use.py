from app.star_order import ordered


def use_order() -> int:
    # Both wildcard sources define `ordered`; Python binds them in order, so
    # the second one wins.
    return ordered(1)
