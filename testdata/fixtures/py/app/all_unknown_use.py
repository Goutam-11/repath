from .all_unknown_pkg import all_narrow


def use_unknown_all() -> int:
    # The source module's __all__ is not a literal, so it stays unknown and
    # the wildcard keeps exposing every public name (#110).
    return all_narrow(1)
