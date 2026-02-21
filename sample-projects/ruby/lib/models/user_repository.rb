# typed: strict
# frozen_string_literal: true

require_relative 'user'

module Models
  class UserRepository
    extend T::Sig

    sig { void }
    def initialize
      @store = T.let({}, T::Hash[Integer, User])
    end

    sig { params(user: User).returns(User) }
    def save(user)
      @store[user.id] = user
      user
    end

    sig { params(id: Integer).returns(T.nilable(User)) }
    def find_by_id(id)
      @store[id]
    end

    sig { returns(T::Array[User]) }
    def find_all
      @store.values
    end
  end
end
