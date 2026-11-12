from .compat_external import exists


def check() -> bool:
    return exists("/")
