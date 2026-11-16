"""Class attributes as receivers, including one inherited in the same file."""


class Cache:
    def read(self) -> int:
        return 1


class BaseRepo:
    cache: Cache


class MemoryRepo(BaseRepo):
    def load(self) -> int:
        return self.cache.read()


def read_cache(repo: MemoryRepo) -> int:
    return repo.load()
