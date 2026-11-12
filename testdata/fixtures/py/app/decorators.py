def logged(func):
    return func


def route(path):
    def decorate(func):
        return func

    return decorate
