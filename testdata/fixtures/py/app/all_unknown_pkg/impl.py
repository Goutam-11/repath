def all_wide(value: int) -> int:
    return value + 1


def all_narrow(value: int) -> int:
    return value + 2


# A non-literal __all__ is unknown, not empty.
__all__ = ["all_wide"] + ["all_narrow"]
