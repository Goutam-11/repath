use crate::User;

pub fn make() -> User {
    User::new(String::new())
}

pub fn literal() -> User {
    User { name: String::new() }
}
