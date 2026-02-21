# typed: strict
# frozen_string_literal: true

require_relative '../core/string_utils'
require_relative '../core/result'
require_relative '../models/user'
require_relative '../models/user_repository'
require_relative '../models/permission'

module App
  class UserService
    extend T::Sig

    sig { params(repository: Models::UserRepository).void }
    def initialize(repository)
      @repository = T.let(repository, Models::UserRepository)
      @next_id = T.let(1, Integer)
    end

    sig { params(name: String, email: String, permission: Models::Permission).returns(Core::Result) }
    def create_user(name, email, permission)
      formatted_name = Core::StringUtils.capitalize_words(name)
      user = Models::User.new(@next_id, formatted_name, email, permission)
      result = user.validate
      if result.success?
        @repository.save(user)
        @next_id += 1
      end
      result
    end

    sig { returns(T::Array[Models::User]) }
    def list_admins
      @repository.find_all.select { |user| user.permission == Models::Permission::Admin }
    end
  end
end
